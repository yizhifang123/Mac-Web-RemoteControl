// TestSource generates synthetic animated BGRA frames and feeds them to the encoder.
// It needs no TCC permission, so it validates the whole encode -> Annex-B -> transport
// path headlessly. `--source screen` swaps in real ScreenCaptureKit frames later.

import Foundation
import CoreVideo
import CoreMedia

final class TestSource {
    private let cfg: H264Encoder.Config
    private let encoder: H264Encoder
    private let maxFrames: Int
    private let audio: OpusAudioEncoder?
    private var pool: CVPixelBufferPool?
    private var running = true
    private var audioTimer: DispatchSourceTimer?
    private var phase = 0.0

    init(cfg: H264Encoder.Config, encoder: H264Encoder, maxFrames: Int = 0,
         audio: OpusAudioEncoder? = nil) {
        self.cfg = cfg
        self.encoder = encoder
        self.maxFrames = maxFrames
        self.audio = audio
    }

    func start() {
        if let audio = audio { startTone(audio) }
        DispatchQueue.global(qos: .userInteractive).async { [weak self] in self?.loop() }
    }

    // A 440 Hz tone through the real Opus path, so the full audio pipe (encode ->
    // framing -> host -> WebRTC -> browser decode) is verifiable without any TCC grant.
    private func startTone(_ audio: OpusAudioEncoder) {
        let t = DispatchSource.makeTimerSource(queue: DispatchQueue(label: "capture.test.audio"))
        t.schedule(deadline: .now(), repeating: .milliseconds(20))
        let step = 2.0 * Double.pi * 440.0 / Double(OpusAudioEncoder.sampleRate)
        t.setEventHandler { [weak self] in
            guard let self else { return }
            let n = OpusAudioEncoder.frameSamples
            var inter = [Float](repeating: 0, count: n * 2)
            for i in 0..<n {
                let s = Float(sin(self.phase)) * 0.2
                self.phase += step
                inter[2 * i] = s; inter[2 * i + 1] = s
            }
            self.phase = fmod(self.phase, 2.0 * Double.pi)
            audio.appendInterleaved(inter)
        }
        t.resume()
        audioTimer = t
        logErr("test source: 440 Hz tone -> opus")
    }

    func stop() { running = false }

    private func loop() {
        guard makePool() else { logErr("test source: pool create failed"); exit(1) }
        logErr("test source: \(cfg.width)x\(cfg.height) @ \(cfg.fps)fps")
        let frameDur = 1.0 / Double(cfg.fps)
        var i = 0
        while running && (maxFrames == 0 || i < maxFrames) {
            let start = DispatchTime.now().uptimeNanoseconds
            if let pb = makeFrame(i) {
                let pts = CMTime(value: Int64(i), timescale: Int32(cfg.fps))
                let dur = CMTime(value: 1, timescale: Int32(cfg.fps))
                encoder.encode(pb, pts: pts, duration: dur)
            }
            i += 1
            let elapsed = Double(DispatchTime.now().uptimeNanoseconds - start) / 1e9
            let remaining = frameDur - elapsed
            if remaining > 0 { usleep(useconds_t(remaining * 1e6)) }
        }
        if maxFrames > 0 {
            encoder.finish()
            usleep(200_000) // let the last encode callbacks drain to stdout
            logErr("test source: emitted \(i) frames, exiting")
            exit(0)
        }
    }

    private func makePool() -> Bool {
        let attrs: [CFString: Any] = [
            kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_32BGRA,
            kCVPixelBufferWidthKey: cfg.width,
            kCVPixelBufferHeightKey: cfg.height,
            kCVPixelBufferIOSurfacePropertiesKey: [:],
        ]
        return CVPixelBufferPoolCreate(nil, nil, attrs as CFDictionary, &pool) == kCVReturnSuccess
    }

    private func makeFrame(_ i: Int) -> CVPixelBuffer? {
        guard let pool = pool else { return nil }
        var pb: CVPixelBuffer?
        guard CVPixelBufferPoolCreatePixelBuffer(nil, pool, &pb) == kCVReturnSuccess,
              let buffer = pb else { return nil }
        CVPixelBufferLockBaseAddress(buffer, [])
        defer { CVPixelBufferUnlockBaseAddress(buffer, []) }
        guard let base = CVPixelBufferGetBaseAddress(buffer) else { return nil }
        let rowBytes = CVPixelBufferGetBytesPerRow(buffer)
        let ptr = base.assumingMemoryBound(to: UInt8.self)
        let w = cfg.width, h = cfg.height
        let red = UInt8((i * 4) & 255)           // background red channel cycles over time
        let barX = (i * 8) % max(1, w)           // a white bar sweeps left to right
        for y in 0..<h {
            let row = ptr + y * rowBytes
            let g = UInt8((y * 255) / max(1, h))
            for x in 0..<w {
                let px = row + x * 4
                if x >= barX && x < barX + 24 {
                    px[0] = 255; px[1] = 255; px[2] = 255
                } else {
                    px[0] = UInt8((x * 255) / max(1, w)) // B: horizontal gradient
                    px[1] = g                             // G: vertical gradient
                    px[2] = red                           // R: time
                }
                px[3] = 255
            }
        }
        return buffer
    }
}
