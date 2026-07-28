// ScreenSource captures the main display with ScreenCaptureKit and feeds each
// complete frame to the encoder. Running this path REQUIRES the Screen Recording
// permission (System Settings -> Privacy & Security -> Screen Recording) granted to
// this binary; without it startCapture throws or delivers only blank frames.

import Foundation
import ScreenCaptureKit
import CoreMedia
import CoreVideo

enum CaptureError: Error { case noDisplay }

final class ScreenSource: NSObject, SCStreamOutput, SCStreamDelegate {
    private let cfg: H264Encoder.Config
    private let encoder: H264Encoder
    private let audio: OpusAudioEncoder?
    private let queue = DispatchQueue(label: "capture.screen")
    private let audioQueue = DispatchQueue(label: "capture.screen.audio")
    private var stream: SCStream?
    private var audioBuffers = 0
    private var audioDropLogged = false

    init(cfg: H264Encoder.Config, encoder: H264Encoder, audio: OpusAudioEncoder? = nil) {
        self.cfg = cfg
        self.encoder = encoder
        self.audio = audio
    }

    func start() async throws {
        let content = try await SCShareableContent.current
        guard let display = content.displays.first else { throw CaptureError.noDisplay }

        let filter = SCContentFilter(display: display, excludingWindows: [])
        let conf = SCStreamConfiguration()
        conf.width = cfg.width
        conf.height = cfg.height
        conf.minimumFrameInterval = CMTime(value: 1, timescale: Int32(cfg.fps))
        conf.pixelFormat = kCVPixelFormatType_32BGRA
        conf.queueDepth = 5
        conf.showsCursor = true
        if audio != nil {
            // System audio (macOS 13+) in the exact format the Opus encoder wants.
            conf.capturesAudio = true
            conf.sampleRate = Int(OpusAudioEncoder.sampleRate)
            conf.channelCount = Int(OpusAudioEncoder.channels)
        }

        let stream = SCStream(filter: filter, configuration: conf, delegate: self)
        try stream.addStreamOutput(self, type: .screen, sampleHandlerQueue: queue)
        if audio != nil {
            try stream.addStreamOutput(self, type: .audio, sampleHandlerQueue: audioQueue)
        }
        try await stream.startCapture()
        self.stream = stream
        logErr("screen capture started: display \(display.width)x\(display.height) -> \(cfg.width)x\(cfg.height) @ \(cfg.fps)fps\(audio != nil ? " + system audio" : "")")
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer, of type: SCStreamOutputType) {
        if type == .audio { handleAudio(sampleBuffer); return }
        guard type == .screen else { return }
        // Only encode frames ScreenCaptureKit marks .complete; skip idle/blank frames.
        guard let attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: false)
                as? [[SCStreamFrameInfo: Any]],
              let attachment = attachments.first,
              let rawStatus = attachment[.status] as? Int,
              let status = SCFrameStatus(rawValue: rawStatus),
              status == .complete else { return }
        guard let pixelBuffer = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }

        let pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer)
        encoder.encode(pixelBuffer, pts: pts, duration: CMTime(value: 1, timescale: Int32(cfg.fps)))
    }

    // Convert one SCK audio sample buffer (float32 PCM, planar or interleaved, from
    // the 48 kHz stereo config above) to interleaved floats for the Opus encoder.
    private func handleAudio(_ sb: CMSampleBuffer) {
        guard let audio = audio else { return }
        guard let fmt = CMSampleBufferGetFormatDescription(sb),
              let asbd = CMAudioFormatDescriptionGetStreamBasicDescription(fmt)?.pointee,
              asbd.mFormatID == kAudioFormatLinearPCM,
              asbd.mFormatFlags & kAudioFormatFlagIsFloat != 0 else {
            if !audioDropLogged {
                audioDropLogged = true
                logErr("audio: DROPPING buffers — unexpected format (not float LPCM); this is the bug, report this log line")
            }
            return
        }
        audioBuffers += 1
        if audioBuffers == 1 || audioBuffers % 500 == 0 {
            let planar = asbd.mFormatFlags & kAudioFormatFlagIsNonInterleaved != 0
            logErr("audio: SCK buffer #\(audioBuffers) — rate=\(Int(asbd.mSampleRate)) ch=\(asbd.mChannelsPerFrame) planar=\(planar) samples=\(CMSampleBufferGetNumSamples(sb))")
        }

        // Two-call pattern: SCK sample buffers need a bigger AudioBufferList than the
        // naive per-channel guess (a fixed 8-buffer list fails with -12737
        // kCMSampleBufferError_ArrayTooSmall), so ask for the required size first.
        var ablSize = 0
        var status = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sb, bufferListSizeNeededOut: &ablSize, bufferListOut: nil, bufferListSize: 0,
            blockBufferAllocator: nil, blockBufferMemoryAllocator: nil,
            flags: 0, blockBufferOut: nil)
        guard status == noErr, ablSize > 0 else {
            if !audioDropLogged {
                audioDropLogged = true
                logErr("audio: DROPPING buffers — ABL size query failed (status \(status)); report this log line")
            }
            return
        }
        let raw = UnsafeMutableRawPointer.allocate(
            byteCount: ablSize, alignment: MemoryLayout<AudioBufferList>.alignment)
        defer { raw.deallocate() }
        let ablPtr = raw.bindMemory(to: AudioBufferList.self, capacity: 1)
        var block: CMBlockBuffer?
        status = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sb, bufferListSizeNeededOut: nil, bufferListOut: ablPtr, bufferListSize: ablSize,
            blockBufferAllocator: kCFAllocatorDefault,
            blockBufferMemoryAllocator: kCFAllocatorDefault,
            flags: 0, blockBufferOut: &block)
        guard status == noErr else {
            if !audioDropLogged {
                audioDropLogged = true
                logErr("audio: DROPPING buffers — ABL copy failed (status \(status)); report this log line")
            }
            return
        }

        withExtendedLifetime(block) {
            let buffers = UnsafeMutableAudioBufferListPointer(ablPtr)
            guard buffers.count > 0 else { return }
            let planar = asbd.mFormatFlags & kAudioFormatFlagIsNonInterleaved != 0
            if planar {
                guard let l = buffers[0].mData?.assumingMemoryBound(to: Float.self) else { return }
                let n = Int(buffers[0].mDataByteSize) / MemoryLayout<Float>.size
                // Mono falls back to duplicating the single plane into both channels.
                let r = buffers.count > 1
                    ? buffers[1].mData?.assumingMemoryBound(to: Float.self) ?? l : l
                var inter = [Float](repeating: 0, count: n * 2)
                for i in 0..<n { inter[2 * i] = l[i]; inter[2 * i + 1] = r[i] }
                audio.appendInterleaved(inter)
            } else {
                guard let d = buffers[0].mData?.assumingMemoryBound(to: Float.self) else { return }
                let n = Int(buffers[0].mDataByteSize) / MemoryLayout<Float>.size
                if Int(asbd.mChannelsPerFrame) == 2 {
                    audio.appendInterleaved(Array(UnsafeBufferPointer(start: d, count: n)))
                } else {
                    var inter = [Float](repeating: 0, count: n * 2)
                    for i in 0..<n { inter[2 * i] = d[i]; inter[2 * i + 1] = d[i] }
                    audio.appendInterleaved(inter)
                }
            }
        }
    }

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        logErr("screen stream stopped: \(error)")
        exit(1)
    }
}
