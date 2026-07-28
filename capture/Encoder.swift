// H264Encoder wraps a VideoToolbox VTCompressionSession tuned for low-latency
// real-time streaming, and converts each encoded frame from AVCC (length-prefixed,
// as VideoToolbox emits) to Annex-B (start-code-prefixed) with SPS/PPS repeated
// in-band on every keyframe — the form Pion's H.264 packetizer and browser decoders
// expect. See docs/ARCHITECTURE.md for the compatibility requirements.

import Foundation
import VideoToolbox
import CoreMedia
import CoreVideo

enum EncoderError: Error { case sessionCreate(OSStatus) }

final class H264Encoder {
    struct Config {
        var width: Int
        var height: Int
        var fps: Int
        var bitrate: Int
        var gopSeconds: Double
        var allowBFrames: Bool
    }

    private var session: VTCompressionSession!
    private let cfg: Config
    private let onFrame: (Data, UInt32) -> Void
    private let startCode: [UInt8] = [0, 0, 0, 1]
    private var lastPTS: CMTime = .invalid
    private let keyLock = NSLock()
    private var forceKeyframeNext = false

    init(config: Config, onFrame: @escaping (Data, UInt32) -> Void) throws {
        self.cfg = config
        self.onFrame = onFrame

        // Prefer the low-latency rate controller + hardware encoder; fall back
        // gracefully if a given specification is unsupported on this machine.
        let specs: [[CFString: Any]?] = [
            [kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder: true,
             kVTVideoEncoderSpecification_EnableLowLatencyRateControl: true],
            [kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder: true],
            nil,
        ]
        var created: VTCompressionSession?
        var lastStatus: OSStatus = noErr
        for (idx, spec) in specs.enumerated() {
            var s: VTCompressionSession?
            let st = VTCompressionSessionCreate(
                allocator: kCFAllocatorDefault,
                width: Int32(config.width),
                height: Int32(config.height),
                codecType: kCMVideoCodecType_H264,
                encoderSpecification: spec as CFDictionary?,
                imageBufferAttributes: nil,
                compressedDataAllocator: nil,
                outputCallback: nil,
                refcon: nil,
                compressionSessionOut: &s)
            lastStatus = st
            if st == noErr, let s = s {
                created = s
                break
            }
            logErr("encoder spec \(idx) unsupported (status \(st)); falling back")
        }
        guard let session = created else { throw EncoderError.sessionCreate(lastStatus) }
        self.session = session

        setBool(kVTCompressionPropertyKey_RealTime, true)
        setBool(kVTCompressionPropertyKey_AllowFrameReordering, config.allowBFrames)
        set(kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel)
        set(kVTCompressionPropertyKey_AverageBitRate, NSNumber(value: config.bitrate))
        set(kVTCompressionPropertyKey_ExpectedFrameRate, NSNumber(value: config.fps))
        let gopFrames = max(1, Int(config.gopSeconds * Double(config.fps)))
        set(kVTCompressionPropertyKey_MaxKeyFrameInterval, NSNumber(value: gopFrames))
        set(kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, NSNumber(value: config.gopSeconds))
        // Cap bursts so a keyframe does not blow past the link budget: peak ~2x avg / sec.
        let bytesPerSec = config.bitrate / 8
        set(kVTCompressionPropertyKey_DataRateLimits, [NSNumber(value: bytesPerSec * 2), NSNumber(value: 1)] as CFArray)

        VTCompressionSessionPrepareToEncodeFrames(session)
        logErr("encoder ready (\(config.width)x\(config.height) @ \(config.fps)fps, \(config.bitrate)bps)")
    }

    // requestKeyframe forces the next encoded frame to be an IDR. Phase 2c wires this
    // to RTCP PLI so a (re)connecting browser recovers immediately.
    func requestKeyframe() {
        keyLock.lock(); forceKeyframeNext = true; keyLock.unlock()
    }

    func encode(_ pixelBuffer: CVPixelBuffer, pts: CMTime, duration: CMTime) {
        keyLock.lock(); let force = forceKeyframeNext; forceKeyframeNext = false; keyLock.unlock()
        var props: CFDictionary?
        if force { props = [kVTEncodeFrameOptionKey_ForceKeyFrame: true] as CFDictionary }

        let status = VTCompressionSessionEncodeFrame(
            session,
            imageBuffer: pixelBuffer,
            presentationTimeStamp: pts,
            duration: duration,
            frameProperties: props,
            infoFlagsOut: nil) { [weak self] status, _, sampleBuffer in
                guard let self = self else { return }
                if status != noErr { logErr("encode callback status \(status)"); return }
                guard let sb = sampleBuffer else { return }
                self.emit(sb)
            }
        if status != noErr { logErr("encodeFrame status \(status)") }
    }

    func finish() {
        VTCompressionSessionCompleteFrames(session, untilPresentationTimeStamp: .invalid)
    }

    // emit converts one encoded sample to Annex-B and hands it to onFrame with a
    // per-frame duration derived from the presentation-timestamp delta.
    private func emit(_ sb: CMSampleBuffer) {
        guard CMSampleBufferDataIsReady(sb),
              let fmt = CMSampleBufferGetFormatDescription(sb),
              let block = CMSampleBufferGetDataBuffer(sb) else { return }

        var out = Data()
        if isKeyframe(sb) {
            for ps in parameterSets(fmt) {
                out.append(contentsOf: startCode)
                out.append(ps)
            }
        }

        var lengthAtOffset = 0, totalLength = 0
        var dataPointer: UnsafeMutablePointer<Int8>?
        guard CMBlockBufferGetDataPointer(block, atOffset: 0,
                                          lengthAtOffsetOut: &lengthAtOffset,
                                          totalLengthOut: &totalLength,
                                          dataPointerOut: &dataPointer) == noErr,
              let dp = dataPointer else { return }
        let base = UnsafeRawPointer(dp)

        // AVCC: repeated [4-byte big-endian NAL length][NAL bytes]. Swap each length
        // prefix for an Annex-B start code.
        var offset = 0
        while offset + 4 <= totalLength {
            var nalLen: UInt32 = 0
            memcpy(&nalLen, base + offset, 4)
            nalLen = CFSwapInt32BigToHost(nalLen)
            offset += 4
            let n = Int(nalLen)
            if n <= 0 || offset + n > totalLength { break }
            out.append(contentsOf: startCode)
            out.append(Data(bytes: base + offset, count: n))
            offset += n
        }

        let pts = CMSampleBufferGetPresentationTimeStamp(sb)
        var durMicros = UInt32(1_000_000 / max(1, cfg.fps))
        if lastPTS.isValid && CMTimeCompare(pts, lastPTS) > 0 {
            let secs = CMTimeGetSeconds(CMTimeSubtract(pts, lastPTS))
            if secs > 0 { durMicros = UInt32(max(1.0, secs * 1_000_000)) }
        }
        lastPTS = pts
        onFrame(out, durMicros)
    }

    private func isKeyframe(_ sb: CMSampleBuffer) -> Bool {
        guard let arr = CMSampleBufferGetSampleAttachmentsArray(sb, createIfNecessary: false)
                as? [[CFString: Any]], let first = arr.first else { return true }
        let notSync = first[kCMSampleAttachmentKey_NotSync] as? Bool ?? false
        return !notSync
    }

    private func parameterSets(_ fmt: CMFormatDescription) -> [Data] {
        var count = 0
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, parameterSetIndex: 0,
            parameterSetPointerOut: nil, parameterSetSizeOut: nil,
            parameterSetCountOut: &count, nalUnitHeaderLengthOut: nil)
        var sets: [Data] = []
        for idx in 0..<count {
            var ptr: UnsafePointer<UInt8>?
            var size = 0
            if CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, parameterSetIndex: idx,
                parameterSetPointerOut: &ptr, parameterSetSizeOut: &size,
                parameterSetCountOut: nil, nalUnitHeaderLengthOut: nil) == noErr, let ptr = ptr {
                sets.append(Data(bytes: ptr, count: size))
            }
        }
        return sets
    }

    private func setBool(_ key: CFString, _ value: Bool) {
        VTSessionSetProperty(session, key: key, value: (value ? kCFBooleanTrue : kCFBooleanFalse))
    }
    private func set(_ key: CFString, _ value: CFTypeRef) {
        VTSessionSetProperty(session, key: key, value: value)
    }
}
