// capture — the macOS screen-capture + H.264 encode helper.
//
// It captures (real screen or a synthetic test pattern), encodes to low-latency
// H.264 with VideoToolbox, and writes framed Annex-B access units to STDOUT for the
// Go host to feed into a Pion WebRTC track. ALL logging goes to STDERR so it never
// corrupts the video stream on stdout.
//
// Framing (default, --framing length): each frame is
//     [uint8 kind][uint32 payloadLength big-endian][uint32 durationMicros big-endian][payload]
// kind 0 = H.264 video access unit, kind 1 = Opus audio packet (Phase 5).
// --framing raw writes concatenated Annex-B VIDEO only (a valid .h264 elementary
// stream for validating the bitstream with ffprobe/ffmpeg; audio is dropped).
//
// Usage:
//   capture --source test   --framing raw --fps 30 --bitrate 4000000 > out.h264
//   capture --source screen --framing length --width 1920 --height 1080 --fps 30

import Foundation
import Dispatch
import CoreGraphics

// MARK: - stderr logging (never touch stdout)

func logErr(_ message: String) {
    FileHandle.standardError.write(Data(("[capture] " + message + "\n").utf8))
}

// MARK: - framed stdout writer

final class FrameWriter {
    private let handle = FileHandle.standardOutput
    private let queue = DispatchQueue(label: "capture.writer")
    private let raw: Bool

    init(raw: Bool) { self.raw = raw }

    // kind 0 = video access unit, 1 = opus audio packet. The queue serializes writers
    // (video and audio arrive from different threads) so frames never interleave.
    func write(_ payload: Data, durationMicros: UInt32, kind: UInt8 = 0) {
        queue.sync {
            do {
                if raw {
                    if kind != 0 { return } // raw mode is a bare .h264 stream
                    try handle.write(contentsOf: payload)
                    return
                }
                var header = Data(capacity: 9)
                header.append(kind)
                header.append(bigEndian: UInt32(payload.count))
                header.append(bigEndian: durationMicros)
                try handle.write(contentsOf: header)
                try handle.write(contentsOf: payload)
            } catch {
                // Broken pipe: the Go host stopped reading. Exit quietly.
                logErr("stdout closed (\(error)); exiting")
                exit(0)
            }
        }
    }
}

extension Data {
    mutating func append(bigEndian value: UInt32) {
        append(UInt8((value >> 24) & 0xff))
        append(UInt8((value >> 16) & 0xff))
        append(UInt8((value >> 8) & 0xff))
        append(UInt8(value & 0xff))
    }
}

// MARK: - args

struct AppConfig {
    var width = 1920
    var height = 1080
    var fps = 30
    // Kept in step with the Go host's default (internal/host/host.go); the helper is
    // normally launched with explicit flags, so these only apply when it is run by hand.
    var bitrate = 12_000_000
    var gopSeconds = 2.0
    var bframes = false
    var source = "test"     // test | screen
    var framing = "length"  // length | raw
    var maxFrames = 0       // 0 = run until killed; >0 = encode N frames then exit (test)
    var input = "off"       // off | on | dry — stdin input handling (Phase 3)
    var audio = false       // capture system audio -> Opus (Phase 5)
}

func parseArgs() -> AppConfig {
    var c = AppConfig()
    let args = Array(CommandLine.arguments.dropFirst())
    var i = 0
    func value() -> String? { i + 1 < args.count ? args[i + 1] : nil }
    while i < args.count {
        switch args[i] {
        case "--width":       if let v = value(), let n = Int(v) { c.width = n }; i += 1
        case "--height":      if let v = value(), let n = Int(v) { c.height = n }; i += 1
        case "--fps":         if let v = value(), let n = Int(v) { c.fps = n }; i += 1
        case "--bitrate":     if let v = value(), let n = Int(v) { c.bitrate = n }; i += 1
        case "--gop-seconds": if let v = value(), let n = Double(v) { c.gopSeconds = n }; i += 1
        case "--bframes":     c.bframes = true
        case "--source":      if let v = value() { c.source = v }; i += 1
        case "--framing":     if let v = value() { c.framing = v }; i += 1
        case "--max-frames":  if let v = value(), let n = Int(v) { c.maxFrames = n }; i += 1
        case "--input":       if let v = value() { c.input = v }; i += 1
        case "--audio":       if let v = value() { c.audio = (v == "on") }; i += 1
        default: logErr("ignoring unknown arg: \(args[i])")
        }
        i += 1
    }
    return c
}

// MARK: - main

signal(SIGPIPE, SIG_IGN) // handle broken-pipe as a write error, not a fatal signal

var cfg = parseArgs()

// Screen capture MUST match the display's aspect ratio: a mismatched target makes
// ScreenCaptureKit letterbox/pillarbox inside the encoded frame, and those bars break
// the client's normalized 0..1 coordinate mapping (cursor lands offset from clicks).
// Width stays the quality knob; height is derived from the display (even, for 4:2:0).
if cfg.source == "screen" {
    let b = CGDisplayBounds(CGMainDisplayID())
    if b.width > 0 {
        let h = Int((Double(cfg.width) * Double(b.height) / Double(b.width) / 2).rounded()) * 2
        if h != cfg.height {
            logErr("display is \(Int(b.width))x\(Int(b.height)) pt — adjusting capture \(cfg.width)x\(cfg.height) -> \(cfg.width)x\(h) to match its aspect")
            cfg.height = h
        }
    }
}

logErr("start: source=\(cfg.source) framing=\(cfg.framing) \(cfg.width)x\(cfg.height)@\(cfg.fps) max-frames=\(cfg.maxFrames)")
let writer = FrameWriter(raw: cfg.framing == "raw")

let encoderConfig = H264Encoder.Config(
    width: cfg.width, height: cfg.height, fps: cfg.fps,
    bitrate: cfg.bitrate, gopSeconds: cfg.gopSeconds, allowBFrames: cfg.bframes)

let encoder: H264Encoder
do {
    encoder = try H264Encoder(config: encoderConfig) { data, durationMicros in
        writer.write(data, durationMicros: durationMicros)
    }
} catch {
    logErr("encoder init failed: \(error)")
    exit(1)
}

// These must be retained for the whole process lifetime. start() dispatches work to
// a background queue / Task and returns immediately; a local would be deallocated the
// moment this scope ends, and (with [weak self]) the dispatched work would silently
// no-op — the process would then sit in dispatchMain() forever doing nothing.
var testSource: TestSource?
var screenSource: ScreenSource?
var inputReader: InputReader?
var audioEncoder: OpusAudioEncoder?

// Audio is optional and non-fatal: a broken opus setup degrades to video-only.
if cfg.audio {
    do {
        audioEncoder = try OpusAudioEncoder(bitrate: 128_000) { data, durMicros in
            writer.write(data, durationMicros: durMicros, kind: 1)
        }
    } catch {
        logErr("audio: encoder init failed (\(error)); continuing without audio")
    }
}

// Input injection is off unless explicitly enabled (view-only is the safe default).
// "dry" opens the reader but posts nothing — used to verify decoding safely.
if cfg.input == "on" || cfg.input == "dry" {
    inputReader = InputReader(injector: InputInjector(dryRun: cfg.input == "dry"))
    inputReader?.start()
} else {
    logErr("input: disabled (view-only)")
}

switch cfg.source {
case "test":
    let source = TestSource(cfg: encoderConfig, encoder: encoder, maxFrames: cfg.maxFrames,
                            audio: audioEncoder)
    testSource = source
    source.start()
case "screen":
    let source = ScreenSource(cfg: encoderConfig, encoder: encoder, audio: audioEncoder)
    screenSource = source
    Task {
        do { try await source.start() }
        catch { logErr("screen capture failed to start: \(error)"); exit(1) }
    }
default:
    logErr("unknown --source \(cfg.source) (want test|screen)")
    exit(2)
}

dispatchMain()
