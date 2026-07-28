# Test media

`testsrc.ivf` is a generated VP8 test pattern (a moving clock/gradient) used by the
Phase 1 host to prove the WebRTC pipe. It is git-ignored — regenerate with:

```sh
./dev.sh media
```

which runs:

```sh
ffmpeg -f lavfi -i testsrc2=size=1280x720:rate=30 -t 8 \
  -c:v libvpx -b:v 3M -deadline realtime -cpu-used 4 -pix_fmt yuv420p -an media/testsrc.ivf
```

VP8 is used here (not H.264) so open-source / headless Chromium — which often omits the
proprietary H.264 decoder — can reliably render it during automated QA. Phase 2 switches
the live track to H.264 from VideoToolbox and validates decode on the real Chromebook.
