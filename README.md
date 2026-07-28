# Mac Web RemoteControl

Control a Mac from any browser — including a locked-down school Chromebook — over
WebRTC. Video and audio travel **peer-to-peer**; only a tiny signaling handshake goes
through a tunnel, so there are **no inbound ports open on your network**.

Built from scratch as a learning project: WebRTC, hardware video encoding, macOS
system APIs, NAT traversal, and security modeling. It is not trying to beat Parsec on
frame pacing — it is trying to be a complete, understandable, self-hosted system.

```
Mac — ONE Go binary + one Swift helper
 ├─ ScreenCaptureKit capture ─┐
 ├─ VideoToolbox H.264 encode ├─ Swift helper (the macOS bits, one permission boundary)
 ├─ system audio → Opus       │
 ├─ CGEvent input injection  ─┘
 ├─ Pion WebRTC ─── media flows PEER-TO-PEER (UDP) via STUN ───┐
 └─ signaling + embedded web client (127.0.0.1:9000)           │
        │ only this rides the tunnel                           │
   cloudflared → your own domain (password gate)               ▼
                                          Browser: <video> + RTCPeerConnection
                                          + Pointer Lock + key capture
```

## What works

- **Live screen** at 1280×N, H.264 via VideoToolbox, hardware-encoded, low latency.
  Capture height is derived from your display's aspect ratio automatically.
- **System audio** as an Opus track (48 kHz stereo), so videos and games have sound.
- **Mouse and keyboard injection** — click, drag, scroll, type, modifiers.
  **View-only is the default**; control requires an explicit flag.
- **Two input modes.** *Desktop* sends absolute coordinates. *Game* uses Pointer Lock
  and sends relative deltas, so FPS/third-person cameras behave, with a sensitivity
  slider and Esc forwarded to the app rather than eaten by the browser.
- **A password gate** in front of everything — the client page and the WebSocket alike.
- **One binary, one command.** The web client is embedded; `play` needs nothing beside
  it but the capture helper.

## Requirements

- **Apple Silicon Mac, macOS 13+** (ScreenCaptureKit + VideoToolbox). Not portable to
  Linux/Windows — roughly half of this is macOS-specific by design.
- **Go 1.24+**, **Xcode command line tools** (for `swiftc`), and **libopus**
  (`brew install opus`).
- A browser on the client side. Chrome/Chromium is the best target; Game mode's
  keyboard-lock relies on a Chrome-family API.
- Optional, for remote access: a domain on Cloudflare and `cloudflared`.

## Quick start

Full detail in **[docs/INSTALL.md](docs/INSTALL.md)** — this is the short version.

```sh
# 1. Toolchain (Homebrew required)
brew install go opus
xcode-select --install

# 2. Clone and verify your machine has everything
git clone https://github.com/yizhifang123/Mac-Web-RemoteControl.git
cd Mac-Web-RemoteControl
./dev.sh doctor

# 3. Build
./dev.sh build

# 4. Smoke test — synthetic video + tone, needs NO macOS permissions
./dev.sh run -source screen-test

# 5. The real thing
./dev.sh run -allow-input
```

`./dev.sh doctor` checks every prerequisite and prints the exact command to fix
whatever is missing. Start there if anything goes wrong.

The first run prints a password **once** — save it. Only a bcrypt hash is stored, so it
cannot be recovered (`-new-password` generates a new one, `-set-password` sets your own).

Then open <http://127.0.0.1:9000> and sign in.

**macOS will ask for two permissions**, both for `bin/capture`: **Screen Recording** to
see the screen, and **Accessibility** to inject input. If the prompts don't appear —
common for command-line binaries — add `bin/capture` by full path in System Settings →
Privacy & Security, then restart the server. macOS only re-reads permissions at start.

> Testing remote control on the same Mac you're typing on is confusing: the video
> mirrors itself and your input fights the injected input. Use a second device. To
> reach it from your LAN, run with `-addr 0.0.0.0:9000` — and read
> [docs/SECURITY.md](docs/SECURITY.md) before leaving it that way.

### Useful flags

| Flag | Meaning |
|---|---|
| *(none)* | **View-only** — the input channel is never even created |
| `-allow-input` | Enable mouse/keyboard control |
| `-input-dry` | Decode and log input but inject nothing (safe testing) |
| `-width 1920` | Capture width; height follows your display's aspect |
| `-fps 60` | Frame rate |
| `-bitrate 15000000` | Encoder bitrate in bits/sec |
| `-audio=false` | Disable the audio track |
| `-source screen-test` | Synthetic video + tone — no permissions needed |
| `-addr 0.0.0.0:9000` | Bind for LAN testing (read the security docs first) |
| `-set-password` | Read a new password from stdin and exit |
| `-new-password` | Generate a random password, print it, and exit |

## Hosting it on your own domain

Media is peer-to-peer, but the signaling handshake has to be reachable. A **Cloudflare
Tunnel** is the recommended route: no port forwarding, no static IP, nothing exposed on
your router.

**[docs/TUNNEL.md](docs/TUNNEL.md)** is a complete walkthrough — creating the tunnel,
the ingress config, DNS, and verifying it's actually locked down.

Whatever you use, the rules that matter: keep the server bound to `127.0.0.1`, put the
tunnel in front, and never disable the password gate on a network-reachable bind.

## Documentation

- **[docs/INSTALL.md](docs/INSTALL.md)** — step-by-step setup, macOS permissions, and
  how to actually use it once running.
- **[docs/TUNNEL.md](docs/TUNNEL.md)** — putting it on your own domain safely.
- **[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)** — black video, no audio, input
  doing nothing, 502s, connection failures.
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how the pieces fit, the wire
  protocols, and why certain non-obvious choices were made.
- **[docs/SECURITY.md](docs/SECURITY.md)** — the threat model. **Read this before
  exposing anything.** This tool types into your Mac; that makes it remote code
  execution by design.

## Honest limitations

- **Latency won't match Parsec.** They have a decade of tuning; this is a hobby build.
- **No adaptive bitrate yet.** Quality is set at launch, so a degrading network
  stutters instead of gracefully softening.
- **No quality presets yet.** No mid-session Game/Movie/Desktop switching.
- **One viewer at a time.** The signaling hub pairs exactly one host with one browser.
- **A single password is the whole perimeter.** There is no per-user auth, no 2FA, and
  no audit log. For anything beyond personal use, put a real identity provider
  (Cloudflare Access, an OAuth proxy) in front.
- **Direct UDP is not guaranteed.** On restrictive networks that block UDP, media has
  no fallback path yet — TURN support is not implemented. If your network forces a
  relay, this will not connect.
- **Not signed or notarized.** You build it yourself and grant permissions to a
  locally-built binary.

## License

MIT — see [LICENSE](LICENSE).
