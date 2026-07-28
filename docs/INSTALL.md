# Getting started

From nothing to controlling your Mac from another device. Budget 10 minutes.

This is macOS-only and Apple Silicon–only, and roughly half the code is macOS system
APIs, so there is no Linux or Windows port.

## 1. Install the toolchain

If you don't have Homebrew:

```sh
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

Then the three things this needs:

```sh
brew install go opus
xcode-select --install
```

- **go** builds the server.
- **opus** is the audio codec the Swift helper links against.
- **xcode-select** installs the Command Line Tools, which provide `swiftc` and the
  macOS SDK. The full Xcode app is *not* required; if `swiftc` still fails afterwards,
  install Xcode and run `sudo xcode-select -s /Applications/Xcode.app`.

## 2. Clone and check

```sh
git clone https://github.com/yizhifang123/Mac-Web-RemoteControl.git
cd Mac-Web-RemoteControl
./dev.sh doctor
```

`doctor` verifies every prerequisite and prints the exact command to fix anything
missing. Don't move on until it says "All set."

## 3. Build

```sh
./dev.sh build
```

This produces `bin/play` (the server, with the web client embedded) and `bin/capture`
(the Swift helper). The helper is ad-hoc code-signed with a stable identifier — that's
what lets macOS remember your permission grants across rebuilds instead of asking again
every time.

## 4. Smoke test — no permissions needed

Before dealing with macOS permissions, confirm the whole pipeline works using a
synthetic video source and a generated tone:

```sh
./dev.sh run -source screen-test
```

It prints a password **once**. Copy it now — only a bcrypt hash is stored, so it
cannot be recovered later (`./dev.sh run -new-password` generates a new one; restart
the server afterwards, since the config is only read at startup).

Open <http://127.0.0.1:9000>, sign in, and you should see a moving test pattern. Click
the 🔇 button for the test tone. If that works, WebRTC, the encoder, the audio path,
and the auth gate are all correct, and anything that breaks next is a permissions
problem rather than a code problem.

Stop it with `Ctrl-C`.

## 5. Grant macOS permissions

Now run it for real:

```sh
./dev.sh run -allow-input
```

macOS will ask for two separate permissions, both required and both for `bin/capture`:

| Permission | Why | Where |
|---|---|---|
| **Screen Recording** | to see the screen | System Settings → Privacy & Security → Screen Recording |
| **Accessibility** | to inject mouse and keyboard input | System Settings → Privacy & Security → Accessibility |

**If a prompt doesn't appear** — common for command-line binaries — add it manually:
open that pane, click **+**, press <kbd>⌘</kbd><kbd>⇧</kbd><kbd>G</kbd>, paste the full
path to `bin/capture` (run `pwd` in the repo to get it), and enable the toggle.

Then **stop and restart** the server. macOS only re-reads permissions when the process
starts.

You'll know Screen Recording worked when the video shows your actual desktop. You'll
know Accessibility worked when clicks and typing land. Until Accessibility is granted,
input silently does nothing — the events post but macOS drops them.

> Want to check input is arriving without it touching your Mac? Add `-input-dry`. It
> decodes and logs every event and injects nothing.

## 6. Connect from another device

Testing remote control on the same Mac you're typing on is confusing — the video
mirrors itself and your input fights the injected input. Use a second device.

On your local network, bind wider so other devices can reach it:

```sh
./dev.sh run -allow-input -addr 0.0.0.0:9000
```

It prints the LAN URLs to open. macOS may pop a firewall prompt the first time — allow
it.

> **Read this before leaving it that way.** A wildcard bind exposes an
> input-injection endpoint to everyone on your network. The password gate still
> applies, but go back to the default loopback bind when you're done testing. For real
> remote access use a tunnel instead — see [TUNNEL.md](TUNNEL.md).

## 7. Put it on your own domain

Once it works locally, [TUNNEL.md](TUNNEL.md) walks through exposing it at your own
hostname with no open inbound ports.

## Using it

- **Click the video** to take control. **Esc** releases.
- **Desktop / Game** toggle: Desktop sends absolute cursor positions. Game grabs
  pointer lock and sends relative deltas for FPS-style cameras — in Game mode Esc is
  forwarded to the app, and you *hold* Esc to release control.
- **Sensitivity slider** scales mouse movement in Game mode.
- **🔇 / 🔊** toggles audio. Browsers force video to start muted, so sound always needs
  one click.

## Common options

```sh
./dev.sh run                            # view-only — no input channel exists at all
./dev.sh run -allow-input               # enable control
./dev.sh run -allow-input -width 1920   # higher resolution (height follows your display)
./dev.sh run -allow-input -fps 60 -bitrate 15000000
./dev.sh run -audio=false               # video only
./dev.sh run -set-password              # change the password (stdin; then restart)
```

Height is derived from your display's aspect ratio automatically — set `-width` and
leave height alone, or the picture gets letterboxed and cursor positions drift.

Something not working? See [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
