// OpusAudioEncoder buffers interleaved stereo float32 PCM into exact 20 ms frames and
// encodes them with libopus (via the COpus shim module). Each encoded packet is handed
// to the callback for framing onto stdout as an audio (kind=1) frame.
//
// 48 kHz stereo is what WebRTC's Opus payload negotiates by default, and 20 ms is the
// canonical packet size — one packet per RTP frame, no packetization surprises.

import Foundation
import COpus

enum AudioError: Error { case encoderCreate(Int32) }

final class OpusAudioEncoder {
    static let sampleRate: Int32 = 48_000
    static let channels: Int32 = 2
    static let frameSamples = 960 // 20 ms @ 48 kHz
    static let frameDurationMicros: UInt32 = 20_000

    private let enc: OpaquePointer
    // All buffering/encoding on one serial queue: sources deliver PCM from their own
    // threads, and libopus encoder state is not thread-safe.
    private let queue = DispatchQueue(label: "capture.audio.enc", qos: .userInteractive)
    private var pending: [Float] = []
    private var out = [UInt8](repeating: 0, count: 4096)
    private var packets = 0
    private let onPacket: (Data, UInt32) -> Void

    init(bitrate: Int32, onPacket: @escaping (Data, UInt32) -> Void) throws {
        var err: Int32 = 0
        guard let e = opus_encoder_create(Self.sampleRate, Self.channels,
                                          OPUS_APPLICATION_AUDIO, &err), err == OPUS_OK else {
            throw AudioError.encoderCreate(err)
        }
        enc = e
        self.onPacket = onPacket
        _ = copus_set_bitrate(enc, bitrate)
        _ = copus_set_inband_fec(enc, 1)
        _ = copus_set_packet_loss_perc(enc, 5)
        logErr("audio: opus encoder ready (48 kHz stereo, \(bitrate) bps, 20 ms frames)")
    }

    deinit { opus_encoder_destroy(enc) }

    /// Append interleaved stereo samples (L R L R …); emits packets as frames fill.
    func appendInterleaved(_ samples: [Float]) {
        queue.async { [self] in
            pending.append(contentsOf: samples)
            let need = Self.frameSamples * Int(Self.channels)
            while pending.count >= need {
                let frame = Array(pending.prefix(need))
                pending.removeFirst(need)
                // Both buffers are non-empty, so baseAddress is never nil.
                let n = frame.withUnsafeBufferPointer { fp in
                    out.withUnsafeMutableBufferPointer { op in
                        opus_encode_float(enc, fp.baseAddress!, opus_int32(Self.frameSamples),
                                          op.baseAddress!, opus_int32(op.count))
                    }
                }
                if n > 0 {
                    onPacket(Data(out[0..<Int(n)]), Self.frameDurationMicros)
                    packets += 1
                    if packets % 250 == 0 { logErr("audio: \(packets) opus packets encoded") }
                } else {
                    logErr("audio: opus_encode_float error \(n)")
                }
            }
        }
    }
}
