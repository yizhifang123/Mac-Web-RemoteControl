# Architecture

Two processes, on purpose.

```
┌─ play (Go) ──────────────────────────────────────────────┐
│  signaling server + embedded web client (127.0.0.1:9000) │
│  auth gate                                               │
│  Pion WebRTC: H.264 video track, Opus audio track,       │
│               "input" data channel                       │
└──────────────┬──────────────────────────┬────────────────┘
     stdout    │  framed A/V              │ stdin: validated input lines
┌──────────────▼──────────────────────────▼────────────────┐
│  capture (Swift) — holds the macOS permission grants     │
│  ScreenCaptureKit → VideoToolbox H.264                   │
│  system audio → libopus                                  │
│  CGEvent injection                                       │
└──────────────────────────────────────────────────────────┘
```

**Why two?** The Swift helper is the single place that holds Screen Recording and
Accessibility permission. Keeping it separate means one auditable boundary, and it
accepts commands only from the local Go process over stdin. It is also simply the only
way to reach ScreenCaptureKit, VideoToolbox, and CGEvent.

**Why is everything else one binary?** Signaling, the web client, and the WebRTC host
used to be separate processes that had to be started in the right order. They are now
one command. The web client is `go:embed`-ed, so the binary runs from anywhere.

## Packages

| Path | Role |
|---|---|
| `cmd/play` | The binary. Starts signaling and the host together. |
| `cmd/signal`, `cmd/host` | Thin wrappers to run either half alone, for debugging. |
| `internal/signaling` | WebSocket hub pairing exactly one host with one browser. |
| `internal/host` | WebRTC session: tracks, data channel, input validation. |
| `internal/auth` | The password gate. See [SECURITY.md](SECURITY.md). |
| `capture/` | Swift helper: capture, encode, inject. |
| `web/` | The browser client — two plain HTML files, no build step, no dependencies. |

## Media pipeline

ScreenCaptureKit delivers BGRA frames to a VideoToolbox compression session configured
for real-time, low-latency H.264:

- **`AllowFrameReordering = false`** — no B-frames. B-frames require buffering future
  frames before emitting the current one, which is fatal for interactivity.
- **Constrained Baseline / Main profile**, in-band SPS/PPS repeated on every keyframe,
  Annex-B framing. Browser H.264 decoders are much pickier than files are; a mismatch
  shows as a `<video>` element that stays black forever rather than an error.
- **~2 second GOP.** Keyframes cost bandwidth but bound recovery time after loss.

VideoToolbox emits AVCC (length-prefixed); Pion's packetizer wants Annex-B
(start-code-prefixed), so the encoder converts on the way out.

**Capture resolution follows your display's aspect ratio.** If it did not,
ScreenCaptureKit would letterbox the screen inside the encoded frame, and those black
bars would break the browser's normalized coordinate mapping — clicks would land
progressively further from the cursor toward the edges. Width is the quality knob;
height is derived.

Audio is captured at 48 kHz stereo and encoded with libopus in 20 ms frames — the
canonical WebRTC packet size — with in-band FEC so a lost packet can be concealed.

### Helper → host framing

One length-prefixed stream carries both media types:

```
[uint8 kind][uint32 payloadLength BE][uint32 durationMicros BE][payload]
   kind 0 = H.264 access unit
   kind 1 = Opus packet
```

Host and helper must be rebuilt together when this changes; a mismatched `kind` byte
is a hard error rather than silent corruption.

## Input pipeline

Browser events → JSON over the WebRTC data channel → **validated** → a one-line text
protocol on the helper's stdin → CGEvent.

The wire format is deliberately tiny. Every message is validated before it reaches the
helper (see [SECURITY.md](SECURITY.md)):

| Type | Meaning |
|---|---|
| `m x y f` | Absolute move, normalized 0..1 |
| `r dx dy f` | Relative move (Game mode), pixel deltas |
| `d b x y f` | Button down at a position |
| `D b f` | Button down at the current cursor (Game mode has no position) |
| `u b f` | Button up |
| `w dx dy f` | Scroll |
| `k code f` / `K code f` | Key down / up, by `KeyboardEvent.code` |
| `x` | Release everything |

`f` is a modifier bitmask: `shift 1 | ctrl 2 | alt 4 | meta 8`.

### Two hard-won details

**Modifier state is absolute, never accumulated.** Every message carries the current
modifier bitmask from the browser event. Tracking key-down/key-up transitions instead
seems natural and is a trap: browsers routinely drop `keyup` (focus changes, OS
shortcuts), so a modifier gets stuck — a stuck Cmd turns every `a` into Select All and
every click into a Cmd-click. Reading absolute state self-heals on the very next event.

Modifier *transitions* are still injected as real key events, because games poll key
state directly (shift-lock, sprint) and cannot see event flags.

**Relative mode carries deltas explicitly.** In Game mode the cursor position is
clamped to the display, but the raw deltas ride on the event via
`kCGMouseEventDeltaX/Y`. Games that capture the mouse read per-event deltas, not cursor
position — so the camera keeps turning even when the cursor is pinned at a screen edge.

## Signaling

A WebSocket hub that pairs exactly one `host` with one `client` and relays SDP and ICE
between them verbatim. When both are present it tells the host to make the offer. The
host is always the offerer.

This is the only part that travels through a tunnel. Once ICE completes, audio, video,
and input flow **directly** between the browser and the Mac over UDP, DTLS-encrypted.
The tunnel carries a few kilobytes at session setup and nothing after.

The host reaches the in-process signaling server over loopback WebSocket rather than a
Go channel. That is deliberate: one signaling code path shared with the standalone
binaries, and the hop carries only SDP/ICE at setup — never media.

## Known gaps

- **No TURN fallback.** If a network blocks direct UDP, there is no relay path and the
  connection fails. This is the biggest functional gap.
- **No adaptive bitrate.** The right approach is Pion's congestion-control interceptor
  (`pion/interceptor/pkg/cc`) driving the encoder's bitrate, reading the sender-side
  estimate — not a client `getStats` round-trip.
- **No quality presets.** Resolution, FPS, and profile changes require recreating the
  compression session, which forces a keyframe and a visible blip; only bitrate is
  adjustable in place.
- **One viewer.** The hub is hard-coded to a single pair.
