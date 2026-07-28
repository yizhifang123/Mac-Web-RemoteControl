# Troubleshooting

Start with `./dev.sh doctor` — it catches most setup problems and prints the fix.

The server logs to the terminal it runs in, and the Swift helper's lines are prefixed
`[capture]`. That output usually names the problem directly, so keep it visible.

## Build

**`swiftc: command not found`**
Run `xcode-select --install`. If it persists, install Xcode and point the toolchain at
it: `sudo xcode-select -s /Applications/Xcode.app`.

**`error: libopus not found`**
`brew install opus`. The build finds it via `brew --prefix opus`, so a non-standard
Homebrew prefix is fine, but a manual source install in an unusual place may not be
found.

**`module 'COpus' not found`**
libopus is installed but its headers aren't where the build expects. Check
`brew --prefix opus`/`include/opus/opus.h` exists, then `brew reinstall opus`.

## Video

**The page loads but the video is black, and frames never climb**
Almost always Screen Recording permission. Check the terminal for
`declined TCCs for application, window, display capture`. Grant it in System Settings →
Privacy & Security → Screen Recording (add `bin/capture` by full path with **+** if it
isn't listed), then **restart the server** — macOS only re-reads permissions at process
start.

**Black video but the HUD says `pc:connected` and frames ARE incrementing**
That's an H.264 decode problem rather than a capture problem, and it means your browser
rejected the stream. Try Chrome. Confirm the pipeline itself is fine with
`./dev.sh run -source screen-test`; if the synthetic source displays and the real screen
doesn't, the issue is capture, not decode.

**Video is laggy or stuttery**
Lower the bitrate to fit your uplink (`-bitrate 4000000`), or drop the resolution
(`-width 1280`). There's no adaptive bitrate yet, so a link that can't sustain the
configured rate stutters rather than degrading gracefully. Latency problems are almost
never bandwidth — they're buffering.

**The picture is letterboxed, or the cursor drifts further off the further you move**
Something forced a mismatched aspect ratio. Set only `-width` and let height be derived
from your display.

## Audio

**No sound at all**
Click the **🔇** button — browsers force video to start muted, and no amount of
server-side correctness changes that.

**Still no sound after unmuting**
Look for `[capture] audio:` lines. `audio: N opus packets encoded` means audio is
flowing and the problem is on the browser side. A `DROPPING` line names the reason
instead. If you see neither, the audio track wasn't negotiated — make sure you didn't
pass `-audio=false`, and note that only the `screen` sources capture audio.

macOS system-audio capture rides on the Screen Recording permission — there is no
separate microphone/audio permission for it.

## Input

**Nothing happens when I click or type**
Three things in order. First, did you pass `-allow-input`? Without it the input channel
is never created — that's the safe default, not a bug. Second, click the video once to
take control; the border turns green. Third, grant Accessibility to `bin/capture` and
restart the server — without it, events post and macOS silently discards them.

To confirm input is arriving without letting it touch your Mac, run with `-input-dry`
and watch the log.

**A modifier key seems stuck**
Shouldn't happen — modifier state is sent absolutely on every event specifically to
self-heal this. Press and release the stuck key, or press Esc to release control, which
sends a release-all. If it persists, it's a bug worth reporting.

**Game mode won't lock the pointer**
Pointer Lock requires a real browser tab (it's blocked in some embedded webviews) and a
user gesture — click the video. Keyboard Lock, which lets Esc reach the app instead of
exiting the lock, is Chrome-family only; elsewhere Esc will exit pointer lock instead.

## Connection

**`502 Bad Gateway` at your tunnel hostname**
The server isn't running. Start it — the origin only exists while `./dev.sh run` is
running, which is deliberate. Also confirm your tunnel's ingress port matches the
server's `-addr` port.

**The page loads but the HUD stays `ws:connecting` or the stream never starts**
Your session probably expired (24 hours) — reload and sign in again. If the WebSocket
is rejected outright, whatever sits in front of the server isn't forwarding WebSocket
upgrades.

**`pc:` goes to `failed`, or connects then dies**
ICE couldn't establish a direct path. Some networks block UDP entirely, and there is
**no TURN relay fallback implemented**, so on those networks this cannot connect. Check
`getStats()` in the browser for the candidate pair. On the same LAN it should be
`host`/`host`; across the internet expect `srflx`. If you only ever see `relay`
candidates offered and nothing succeeds, that network needs TURN support this project
doesn't have yet.

**`bind: address already in use`**
Something else holds port 9000 — `lsof -nP -iTCP:9000 -sTCP:LISTEN` shows what. Use
`-addr 127.0.0.1:9001` instead (update your tunnel ingress to match).

## Auth

**I lost the password**
It's stored only as a bcrypt hash and cannot be recovered. Generate a new one with
`./dev.sh run -new-password`, or set your own with `./dev.sh run -set-password`. Both
write the config file and exit — **restart the server** afterwards.

**I changed the password and it still wants the old one**
Restart the server. Credentials are loaded once at startup, so a process that was
already running keeps the old password *and* the old sessions until you restart it.
This matters if you are changing the password because a session may have been stolen:
until the restart, that session is still live.

**Signed out unexpectedly**
Sessions last 24 hours, and changing the password deliberately invalidates every
existing session — from the next server start.

**`too many attempts`**
The per-client lockout: 10 failures from one address triggers a one-minute lockout.
Wait a minute. If instead your login is simply *slow* (a couple of seconds), that's the
global throttle — more than 30 failed attempts a minute are arriving from somewhere, so
every attempt is being paced. It will still let you in with the right password.

## Still stuck?

Open an issue with your macOS version, the output of `./dev.sh doctor`, and the
relevant terminal output — especially any `[capture]` lines.
