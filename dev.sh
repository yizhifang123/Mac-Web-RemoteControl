#!/usr/bin/env bash
# Mac Web RemoteControl dev helper.
#
# Usage: ./dev.sh {doctor|build|run|capture|media|signal|host}
#   doctor — check prerequisites and tell you what's missing (start here)
#   build  — compile everything into bin/
#   run    — the whole remote desktop in one process (this is the normal way)
#   signal/host — the two halves separately, for debugging one at a time
set -euo pipefail
cd "$(dirname "$0")"

# Stable code-signing identifier. macOS ties Screen Recording and Accessibility
# grants to a binary's identity, so keeping this constant means your permissions
# survive rebuilds instead of resetting on every one. Do not make it dynamic.
CODESIGN_ID="local.macwebremotecontrol.capture"

# Locate libopus wherever it lives (Apple Silicon brew, Intel brew, custom prefix).
opus_prefix() {
  if command -v brew >/dev/null 2>&1 && brew --prefix opus >/dev/null 2>&1; then
    brew --prefix opus
  elif [ -d /opt/homebrew/opt/opus ]; then
    echo /opt/homebrew/opt/opus
  elif [ -d /usr/local/opt/opus ]; then
    echo /usr/local/opt/opus
  else
    echo ""
  fi
}

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; printf '      fix: %s\n' "$2"; FAILED=1; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }

case "${1:-}" in
  doctor)
    FAILED=0
    echo "Checking prerequisites..."

    if [ "$(uname -s)" = "Darwin" ]; then
      ok "macOS $(sw_vers -productVersion)"
    else
      bad "not macOS" "this project is macOS-only; there is no Linux/Windows port"
    fi

    if [ "$(uname -m)" = "arm64" ]; then
      ok "Apple Silicon (arm64)"
    else
      warn "architecture $(uname -m) — developed and tested on Apple Silicon only; the swiftc target below assumes arm64"
    fi

    case "$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)" in
      1[3-9]|2[0-9]) ok "macOS 13+ (ScreenCaptureKit available)" ;;
      *) bad "macOS 13 or newer required" "ScreenCaptureKit and system-audio capture need macOS 13+" ;;
    esac

    if command -v go >/dev/null 2>&1; then
      ok "go $(go version | awk '{print $3}')"
    else
      bad "go not found" "brew install go"
    fi

    if command -v swiftc >/dev/null 2>&1; then
      ok "swiftc present"
    else
      bad "swiftc not found" "xcode-select --install"
    fi

    OPUS="$(opus_prefix)"
    if [ -n "$OPUS" ] && [ -f "$OPUS/include/opus/opus.h" ]; then
      ok "libopus ($OPUS)"
    else
      bad "libopus not found" "brew install opus"
    fi

    if command -v ffmpeg >/dev/null 2>&1; then
      ok "ffmpeg (optional — only for ./dev.sh media)"
    else
      warn "ffmpeg not found — optional; only needed to generate the VP8 test clip"
    fi

    if command -v cloudflared >/dev/null 2>&1; then
      ok "cloudflared (optional — for remote access)"
    else
      warn "cloudflared not found — optional; needed only to reach this from outside your network (docs/TUNNEL.md)"
    fi

    if [ -x bin/capture ]; then
      ok "bin/capture is built"
      if codesign -dv bin/capture 2>&1 | grep -q "$CODESIGN_ID"; then
        ok "capture helper signed with the stable identifier"
      else
        warn "capture helper signature differs — rerun ./dev.sh build, or macOS may ask for permissions again"
      fi
    else
      warn "bin/capture not built yet — run ./dev.sh build"
    fi

    if lsof -nP -iTCP:9000 -sTCP:LISTEN >/dev/null 2>&1; then
      warn "port 9000 is in use — stop that process, or pass -addr 127.0.0.1:9001"
    else
      ok "port 9000 is free"
    fi

    echo
    if [ "$FAILED" = "1" ]; then
      echo "Missing prerequisites — see the fixes above, then rerun ./dev.sh doctor"
      exit 1
    fi
    echo "All set. Next:"
    echo "  ./dev.sh build                     # compile"
    echo "  ./dev.sh run -source screen-test   # smoke test — needs NO permissions"
    echo "  ./dev.sh run -allow-input          # the real thing"
    echo
    echo "macOS asks for Screen Recording and Accessibility the first time you capture"
    echo "or inject input. If the prompts don't appear, see docs/INSTALL.md."
    ;;

  build)
    mkdir -p bin
    go build -o bin/play   ./cmd/play
    go build -o bin/signal ./cmd/signal
    go build -o bin/host   ./cmd/host
    echo "built bin/play (+ bin/signal, bin/host for debugging)"
    "$0" capture
    ;;

  capture)
    mkdir -p bin
    OPUS="$(opus_prefix)"
    if [ -z "$OPUS" ]; then
      echo "error: libopus not found. Install it with:  brew install opus" >&2
      echo "       Run ./dev.sh doctor to check everything at once." >&2
      exit 1
    fi
    # COpus module (capture/include) wraps libopus for the Opus audio track.
    swiftc -O -target arm64-apple-macos13.0 capture/*.swift \
      -I capture/include -Xcc -I"$OPUS/include" -L"$OPUS/lib" \
      -framework ScreenCaptureKit -framework VideoToolbox \
      -framework CoreMedia -framework CoreVideo \
      -o bin/capture
    # Ad-hoc sign with a STABLE identifier so macOS keeps the Screen Recording and
    # Accessibility grants across rebuilds instead of treating each build as new.
    codesign --force --sign - --identifier "$CODESIGN_ID" bin/capture 2>/dev/null || true
    echo "built bin/capture"
    ;;

  media)
    mkdir -p media
    if [ -f media/testsrc.ivf ]; then
      echo "media/testsrc.ivf already exists"
    else
      command -v ffmpeg >/dev/null 2>&1 || { echo "error: ffmpeg not installed (brew install ffmpeg)" >&2; exit 1; }
      ffmpeg -hide_banner -loglevel error \
        -f lavfi -i testsrc2=size=1280x720:rate=30 -t 8 \
        -c:v libvpx -b:v 3M -deadline realtime -cpu-used 4 -pix_fmt yuv420p -an \
        media/testsrc.ivf
      echo "wrote media/testsrc.ivf"
    fi
    ;;

  run)    exec ./bin/play   "${@:2}" ;;
  signal) exec ./bin/signal "${@:2}" ;;
  host)   exec ./bin/host   "${@:2}" ;;
  *) echo "usage: ./dev.sh {doctor|build|run|capture|media|signal|host}"; exit 1 ;;
esac
