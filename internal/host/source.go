package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// writeMove takes a validated mouse move ("m" or "r") from the unreliable move
	// channel. Moves are coalesced, never queued: see moveSlot.
	writeMove(m clientInput)
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
	// Sequenced moves (see moveSeq): N orders "m" and "c" moves sent over the
	// unordered move channel; CX/CY are "c"'s running totals of relative motion.
	N  int64   `json:"n"`
	CX float64 `json:"cx"`
	CY float64 `json:"cy"`
}

// translateInput validates one client message and renders it as a single line of the
// helper protocol. It returns ok=false for anything malformed or out of range, so a
// hostile or buggy client cannot smuggle arbitrary bytes onto the helper's stdin.
//
// The modifier bitmask travels with every event so the injector applies absolute
// modifier state (no stuck Cmd/Shift from dropped keyup events).
func translateInput(data []byte) ([]byte, bool) {
	m, ok := decodeInput(data)
	if !ok {
		return nil, false
	}
	line := m.line()
	return line, line != nil // "c" has no line of its own: it goes through moveSeq
}

// decodeInput parses and validates one client message; everything line() renders
// has passed through here first.
func decodeInput(data []byte) (clientInput, bool) {
	var m clientInput
	if len(data) > 256 { // a legit message is tens of bytes
		return m, false
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, false
	}
	if m.F < 0 || m.F > 15 || m.N < 0 {
		return m, false
	}
	switch m.T {
	case "m":
		return m, inUnit(m.X) && inUnit(m.Y)
	case "c":
		return m, m.N > 0 // totals are differenced and the delta clamped in moveSeq
	case "r", "w", "x":
		return m, true // deltas are clamped when rendered
	case "D", "u":
		return m, m.B >= 0 && m.B <= 2
	case "d":
		return m, m.B >= 0 && m.B <= 2 && inUnit(m.X) && inUnit(m.Y)
	case "k", "K":
		return m, validKeyCode(m.C)
	}
	return m, false
}

// line renders a validated message as one helper-protocol command.
func (m clientInput) line() []byte {
	switch m.T {
	case "m":
		return []byte(fmt.Sprintf("m %.5f %.5f %d\n", m.X, m.Y, m.F))
	case "r":
		// Relative mouse move (game mode / pointer lock): pixel deltas, not positions.
		return []byte(fmt.Sprintf("r %.1f %.1f %d\n", clampDelta(m.DX), clampDelta(m.DY), m.F))
	case "D":
		// Button down at the current cursor — game mode has no absolute position.
		return []byte(fmt.Sprintf("D %d %d\n", m.B, m.F))
	case "d":
		// Carry the click position so the press lands exactly where the browser
		// clicked, independent of the last mouse-move.
		return []byte(fmt.Sprintf("d %d %.5f %.5f %d\n", m.B, m.X, m.Y, m.F))
	case "u":
		return []byte(fmt.Sprintf("u %d %d\n", m.B, m.F))
	case "w":
		return []byte(fmt.Sprintf("w %.1f %.1f %d\n", clampScroll(m.DX), clampScroll(m.DY), m.F))
	case "k", "K":
		return []byte(fmt.Sprintf("%s %s %d\n", m.T, m.C, m.F))
	case "x":
		return []byte("x\n") // release everything (buttons + modifiers)
	}
	return nil
}

func inUnit(v float64) bool { return v >= 0 && v <= 1 }

// maxDelta bounds one injected relative move; no legit human motion exceeds this in
// one tick, and it keeps a hostile client from teleport-spamming the cursor.
const maxDelta = 1000

func clampDelta(v float64) float64 {
	return math.Max(-maxDelta, math.Min(maxDelta, v))
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

// helperFailure turns a dead capture helper's exit status into a viewer-legible
// error. Exit code 3 is the helper's "no capturable display" signal (main.swift):
// the display was asleep and could not be woken — the one failure the person at the
// browser can actually act on, so it must not surface as a bare EOF. The helper has
// already exited by the time its stdout hits EOF, so the Wait here returns promptly;
// the deferred cmd.Wait() in stream() then reports "already called", which it ignores.
func helperFailure(cmd *exec.Cmd, readErr error) error {
	var ee *exec.ExitError
	if errors.As(cmd.Wait(), &ee) && ee.ExitCode() == 3 {
		return errors.New("the Mac has no capturable display — its screen is asleep and could not be woken (lid closed?)")
	}
	return fmt.Errorf("read frame header: %w", readErr)
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

	wmu   sync.Mutex // serializes every write to stdin, so lines never interleave
	moves moveSlot
}

func (*screenSource) codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}
}

func (s *screenSource) hasAudio() bool { return s.cfg.audio }

// writeInput forwards one already-validated command line to the helper. Best-effort:
// input arriving before the helper is up (or after it exits) is dropped. A pending
// mouse move is written first, so a click never lands before the move preceding it.
func (s *screenSource) writeInput(line []byte) {
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w == nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for mv := s.moves.take(); mv != nil; mv = s.moves.take() {
		_, _ = w.Write(mv)
	}
	_, _ = w.Write(line)
}

func (s *screenSource) writeMove(m clientInput) { s.moves.put(m) }

// requestKeyframe asks the helper's encoder for an IDR as the next frame. Rides the
// helper's stdin, which is open even in view-only mode for exactly this.
func (s *screenSource) requestKeyframe() { s.writeInput([]byte("kf\n")) }

// pumpMoves writes coalesced moves to the helper as they arrive. While a write is
// blocked (the injector is behind), new moves merge into the pending slot instead of
// queueing — so a backlog collapses to the latest position rather than replaying
// every stale one late, which is what "the cursor retraces my path" looks like.
func (s *screenSource) pumpMoves(ctx context.Context, w io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.moves.wakeCh():
		}
		s.wmu.Lock()
		for mv := s.moves.take(); mv != nil; mv = s.moves.take() {
			_, _ = w.Write(mv)
		}
		s.wmu.Unlock()
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
	if s.inputMode == "on" || s.inputMode == "dry" {
		args = append(args, "--input", s.inputMode)
	}

	cmd := exec.CommandContext(ctx, s.helperPath, args...)
	cmd.Stderr = os.Stderr // surface the helper's [capture] logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// stdin is always open: it carries keyframe requests even in view-only mode. The
	// helper only injects input when started with --input, so this grants nothing.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start capture helper %q: %w", s.helperPath, err)
	}
	s.moves.clear() // a move left over from the previous session must not leak into this one
	s.mu.Lock()
	s.stdin = stdin
	s.mu.Unlock()
	go s.pumpMoves(ctx, stdin)
	defer func() {
		s.mu.Lock()
		if s.stdin == stdin { // a late teardown must not unhook the NEXT session's helper
			s.stdin = nil
		}
		s.mu.Unlock()
		_ = stdin.Close()
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
			return helperFailure(cmd, err)
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
