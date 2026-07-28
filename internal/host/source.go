package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

// maxFrameBytes bounds a single framed access unit so a corrupt length header can
// never trigger a huge allocation. 8 MiB is far above any real 1080p frame.
const maxFrameBytes = 8 << 20

// inputTarget is a video source that can also accept validated input command lines
// (the screen helper does; the VP8 test file does not).
type inputTarget interface {
	writeInput(line []byte)
}

// clientInput is the compact wire message the browser sends over the input data
// channel. Every field is validated in translateInput before anything is forwarded to
// the injector — this is the trust boundary between the browser and CGEvent.
type clientInput struct {
	T  string  `json:"t"` // m|r|d|D|u|w|k|K|x
	X  float64 `json:"x"` // mouse move, normalized 0..1
	Y  float64 `json:"y"`
	B  int     `json:"b"`  // mouse button 0|1|2
	DX float64 `json:"dx"` // scroll deltas (w) or relative-move pixel deltas (r)
	DY float64 `json:"dy"`
	C  string  `json:"c"` // key code (browser KeyboardEvent.code)
	F  int     `json:"f"` // modifier bitmask: shift1|ctrl2|alt4|meta8
}

// translateInput validates one client message and renders it as a single line of the
// helper protocol. It returns ok=false for anything malformed or out of range, so a
// hostile or buggy client cannot smuggle arbitrary bytes onto the helper's stdin.
//
// The modifier bitmask travels with every event so the injector applies absolute
// modifier state (no stuck Cmd/Shift from dropped keyup events).
func translateInput(data []byte) ([]byte, bool) {
	if len(data) > 256 { // a legit message is tens of bytes
		return nil, false
	}
	var m clientInput
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	if m.F < 0 || m.F > 15 {
		return nil, false
	}
	switch m.T {
	case "m":
		if !inUnit(m.X) || !inUnit(m.Y) {
			return nil, false
		}
		return []byte(fmt.Sprintf("m %.5f %.5f %d\n", m.X, m.Y, m.F)), true
	case "r":
		// Relative mouse move (game mode / pointer lock): pixel deltas, not positions.
		return []byte(fmt.Sprintf("r %.1f %.1f %d\n", clampDelta(m.DX), clampDelta(m.DY), m.F)), true
	case "D":
		// Button down at the current cursor — game mode has no absolute position.
		if m.B < 0 || m.B > 2 {
			return nil, false
		}
		return []byte(fmt.Sprintf("D %d %d\n", m.B, m.F)), true
	case "d":
		if m.B < 0 || m.B > 2 || !inUnit(m.X) || !inUnit(m.Y) {
			return nil, false
		}
		// Carry the click position so the press lands exactly where the browser
		// clicked, independent of the last mouse-move.
		return []byte(fmt.Sprintf("d %d %.5f %.5f %d\n", m.B, m.X, m.Y, m.F)), true
	case "u":
		if m.B < 0 || m.B > 2 {
			return nil, false
		}
		return []byte(fmt.Sprintf("u %d %d\n", m.B, m.F)), true
	case "w":
		return []byte(fmt.Sprintf("w %.1f %.1f %d\n", clampScroll(m.DX), clampScroll(m.DY), m.F)), true
	case "k", "K":
		if !validKeyCode(m.C) {
			return nil, false
		}
		return []byte(fmt.Sprintf("%s %s %d\n", m.T, m.C, m.F)), true
	case "x":
		return []byte("x\n"), true // release everything (buttons + modifiers)
	}
	return nil, false
}

func inUnit(v float64) bool { return v >= 0 && v <= 1 }

// clampDelta bounds one relative-move delta; no legit human motion exceeds this in
// one 8ms tick, and it keeps a hostile client from teleport-spamming the cursor.
func clampDelta(v float64) float64 {
	if v > 1000 {
		return 1000
	}
	if v < -1000 {
		return -1000
	}
	return v
}

func clampScroll(v float64) float64 {
	if v > 120 {
		return 120
	}
	if v < -120 {
		return -120
	}
	return v
}

// validKeyCode allows only short alphanumeric tokens (KeyA, ArrowLeft, Digit0, F11).
// Rejecting spaces/newlines/punctuation keeps the single-line helper protocol safe.
func validKeyCode(c string) bool {
	if len(c) == 0 || len(c) > 20 {
		return false
	}
	for _, r := range c {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// frameSource produces a video track's codec and pumps encoded samples into it until
// the context is cancelled. Phase 1 uses ivfSource (VP8 file); Phase 2 uses
// screenSource (live H.264 from the Swift helper). Sources that also capture audio
// (Phase 5) report hasAudio and receive a non-nil Opus track to fill.
type frameSource interface {
	codec() webrtc.RTPCodecCapability
	hasAudio() bool
	stream(ctx context.Context, video, audio *webrtc.TrackLocalStaticSample) error
}

// ivfSource loops a VP8 IVF file — the Phase 1 test path.
type ivfSource struct{ path string }

func (ivfSource) codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
}

func (ivfSource) hasAudio() bool { return false }

func (s ivfSource) stream(ctx context.Context, track, _ *webrtc.TrackLocalStaticSample) error {
	for ctx.Err() == nil {
		if err := playIVFOnce(ctx, track, s.path); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func playIVFOnce(ctx context.Context, track *webrtc.TrackLocalStaticSample, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	ivf, header, err := ivfreader.NewWith(f)
	if err != nil {
		return err
	}
	dur := time.Duration(float64(header.TimebaseNumerator)/float64(header.TimebaseDenominator)*1000) * time.Millisecond
	if dur <= 0 {
		dur = 33 * time.Millisecond
	}
	ticker := time.NewTicker(dur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		frame, _, err := ivf.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			return nil // loop: caller reopens
		}
		if err != nil {
			return err
		}
		if err := track.WriteSample(media.Sample{Data: frame, Duration: dur}); err != nil {
			return err
		}
	}
}

// captureConfig is the encoder configuration passed through to the Swift helper.
// Phase 2b turns these into named presets; Phase 2c adjusts bitrate live.
type captureConfig struct {
	width, height, fps, bitrate int
	gopSeconds                  float64
	bframes                     bool
	audio                       bool // system audio -> Opus track (Phase 5)
}

// screenSource spawns the Swift capture helper and reads its framed H.264 Annex-B
// output. helperSource selects the helper's own --source: "screen" for the live
// display (needs Screen Recording permission) or "test" for the synthetic pattern
// (headless-verifiable, no permission).
//
// When allowInput is set, it also opens the helper's stdin and forwards host-validated
// input command lines to it (Phase 3). It is used by pointer so the stdin handle set
// in stream() is visible to writeInput() called from the data-channel goroutine.
type screenSource struct {
	helperPath   string
	helperSource string // screen | test
	cfg          captureConfig
	inputMode    string // off | on | dry

	mu    sync.Mutex
	stdin io.WriteCloser // set once the helper starts; nil before/after
}

func (*screenSource) codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}
}

func (s *screenSource) hasAudio() bool { return s.cfg.audio }

// writeInput forwards one already-validated command line to the helper. Best-effort:
// input arriving before the helper is up (or after it exits) is dropped.
func (s *screenSource) writeInput(line []byte) {
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w != nil {
		_, _ = w.Write(line)
	}
}

func (s *screenSource) stream(ctx context.Context, video, audio *webrtc.TrackLocalStaticSample) error {
	args := []string{
		"--source", s.helperSource,
		"--framing", "length",
		"--width", strconv.Itoa(s.cfg.width),
		"--height", strconv.Itoa(s.cfg.height),
		"--fps", strconv.Itoa(s.cfg.fps),
		"--bitrate", strconv.Itoa(s.cfg.bitrate),
		"--gop-seconds", strconv.FormatFloat(s.cfg.gopSeconds, 'f', -1, 64),
	}
	if s.cfg.bframes {
		args = append(args, "--bframes")
	}
	if s.cfg.audio {
		args = append(args, "--audio", "on")
	}
	inputOn := s.inputMode == "on" || s.inputMode == "dry"
	if inputOn {
		args = append(args, "--input", s.inputMode)
	}

	cmd := exec.CommandContext(ctx, s.helperPath, args...)
	cmd.Stderr = os.Stderr // surface the helper's [capture] logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stdin io.WriteCloser
	if inputOn {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start capture helper %q: %w", s.helperPath, err)
	}
	if inputOn {
		s.mu.Lock()
		s.stdin = stdin
		s.mu.Unlock()
	}
	defer func() {
		s.mu.Lock()
		s.stdin = nil
		s.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		_ = cmd.Wait() // reaped after ctx cancellation kills it
	}()

	// Framing: [u8 kind][u32 length][u32 durationMicros][payload];
	// kind 0 = H.264 access unit, 1 = Opus packet.
	r := bufio.NewReaderSize(stdout, 1<<20)
	header := make([]byte, 9)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read frame header: %w", err)
		}
		kind := header[0]
		n := binary.BigEndian.Uint32(header[1:5])
		durMicros := binary.BigEndian.Uint32(header[5:9])
		if kind > 1 {
			return fmt.Errorf("unknown frame kind %d (host/helper framing mismatch — rebuild both)", kind)
		}
		if n == 0 || n > maxFrameBytes {
			return fmt.Errorf("frame length %d out of range", n)
		}
		buf := make([]byte, int(n))
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("read frame payload: %w", err)
		}
		track := video
		if kind == 1 {
			if audio == nil {
				continue // audio frame but no negotiated track: drop
			}
			track = audio
		}
		dur := time.Duration(durMicros) * time.Microsecond
		if dur <= 0 {
			dur = 33 * time.Millisecond
		}
		if err := track.WriteSample(media.Sample{Data: buf, Duration: dur}); err != nil {
			return err
		}
	}
}
