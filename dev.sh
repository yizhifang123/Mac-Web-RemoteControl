#!/usr/bin/env bash
# Mac Web RemoteControl dev helper.
# Usage: ./dev.sh {build|run|capture|media|signal|host}
#   run    — the whole remote desktop in one process (this is the normal way)
#   signal/host — the two halves separately, for debugging one at a time
set -euo pipefail
cd "$(dirname "$0")"

case "${1:-}" in
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
    # COpus module (capture/include) wraps Homebrew libopus for the audio track.
    swiftc -O -target arm64-apple-macos13.0 capture/*.swift \
      -I capture/include -Xcc -I/opt/homebrew/include -L/opt/homebrew/lib \
      -framework ScreenCaptureKit -framework VideoToolbox \
      -framework CoreMedia -framework CoreVideo \
      -o bin/capture
    # Ad-hoc sign with a STABLE identifier so the Screen Recording grant sticks to a
    # consistent identity instead of resetting on every rebuild (reduces re-granting).
    codesign --force --sign - --identifier dev.yizhi.play.capture bin/capture 2>/dev/null || true
    echo "built bin/capture"
    ;;
  media)
    mkdir -p media
    if [ -f media/testsrc.ivf ]; then
      echo "media/testsrc.ivf already exists"
    else
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
  *) echo "usage: ./dev.sh {build|run|capture|media|signal|host}"; exit 1 ;;
esac
