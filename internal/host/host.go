// Package host is the media sender. It connects to a signaling server as the "host"
// peer, publishes the Mac's screen (H.264) and system audio (Opus) over WebRTC, and
// forwards validated browser input back to the capture helper.
//
// The signaling server it dials may be a standalone process (cmd/signal) or one
// running in the same binary (cmd/play); either way it speaks the same WebSocket
// protocol, so there is only one code path. See docs/ARCHITECTURE.md.
package host

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// sigMsg is the tiny signaling protocol. Offer/answer carry SDP; candidate carries a
// raw ICECandidateInit forwarded verbatim by the server.
type sigMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

var stun = []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}

// Config is everything the host needs to run its session loop.
type Config struct {
	SignalURL   string
	Token       string // bearer token for the signaling gate (empty only when auth is off)
	Source      string // screen | screen-test | test
	MediaPath   string // VP8 IVF file, source=test only
	CapturePath string // Swift capture helper, source=screen*

	Width, Height, FPS, Bitrate int
	GOPSeconds                  float64
	BFrames                     bool
	Audio                       bool

	AllowInput bool
	InputDry   bool
}

// BindFlags registers the host's capture/input flags on fs and returns a func that
// reads them into a Config once fs has been parsed. Defined once so the standalone
// host (cmd/host) and the combined binary (cmd/play) can never drift apart.
//
// SignalURL is deliberately NOT a flag here: cmd/host takes it from -signal, while
// cmd/play always points the host at the server it started itself.
func BindFlags(fs *flag.FlagSet) func() Config {
	source := fs.String("source", "screen", "video source: screen (live) | screen-test (synthetic H.264) | test (VP8 file)")
	mediaPath := fs.String("media", "media/testsrc.ivf", "VP8 IVF file to loop (source=test)")
	capturePath := fs.String("capture", "bin/capture", "path to the Swift capture helper (source=screen*)")
	width := fs.Int("width", 1280, "capture width (height follows the display's aspect)")
	height := fs.Int("height", 720, "capture height (overridden to match display aspect on source=screen)")
	fps := fs.Int("fps", 30, "capture frame rate")
	bitrate := fs.Int("bitrate", 8_000_000, "encoder bitrate (bits/sec)")
	gop := fs.Float64("gop", 2.0, "keyframe interval (seconds)")
	bframes := fs.Bool("bframes", false, "allow B-frames (higher latency; Movie preset)")
	audio := fs.Bool("audio", true, "capture system audio as an Opus track (screen sources)")
	allowInput := fs.Bool("allow-input", false, "enable mouse/keyboard control (default: view-only)")
	inputDry := fs.Bool("input-dry", false, "with -allow-input: decode+log input but inject nothing (safe testing)")

	return func() Config {
		return Config{
			Source: *source, MediaPath: *mediaPath, CapturePath: *capturePath,
			Width: *width, Height: *height, FPS: *fps, Bitrate: *bitrate,
			GOPSeconds: *gop, BFrames: *bframes, Audio: *audio,
			AllowInput: *allowInput, InputDry: *inputDry,
		}
	}
}

// Run drives the host until ctx is cancelled, reconnecting to signaling on failure.
func Run(ctx context.Context, cfg Config) error {
	inputMode := "off"
	if cfg.AllowInput {
		inputMode = "on"
		if cfg.InputDry {
			inputMode = "dry"
		}
	}

	src, err := buildSource(cfg.Source, cfg.MediaPath, cfg.CapturePath, inputMode, captureConfig{
		width: cfg.Width, height: cfg.Height, fps: cfg.FPS, bitrate: cfg.Bitrate,
		gopSeconds: cfg.GOPSeconds, bframes: cfg.BFrames, audio: cfg.Audio,
	})
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	hostURL, err := withRole(cfg.SignalURL, "host")
	if err != nil {
		return fmt.Errorf("bad -signal url %q: %w", cfg.SignalURL, err)
	}
	control := map[string]string{"off": "view-only", "on": "INPUT ENABLED", "dry": "input DRY-RUN (no injection)"}[inputMode]
	log.Printf("host video source: %s (%s), %s", cfg.Source, src.codec().MimeType, control)

	enableInput := inputMode != "off"
	for {
		if err := connectAndServe(ctx, hostURL, cfg.Token, src, enableInput); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("signaling ended: %v; reconnecting in 2s", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// buildSource maps the -source flag to a frameSource implementation.
func buildSource(source, mediaPath, capturePath, inputMode string, cfg captureConfig) (frameSource, error) {
	switch source {
	case "test":
		return ivfSource{path: mediaPath}, nil // VP8 file: no input path
	case "screen":
		return &screenSource{helperPath: capturePath, helperSource: "screen", cfg: cfg, inputMode: inputMode}, nil
	case "screen-test":
		return &screenSource{helperPath: capturePath, helperSource: "test", cfg: cfg, inputMode: inputMode}, nil
	default:
		return nil, fmt.Errorf("unknown -source %q (want test | screen | screen-test)", source)
	}
}

// withRole returns raw with the ?role= query parameter set (the signaling server
// requires it to route between the host and the browser).
func withRole(raw, role string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("role", role)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// connectAndServe dials signaling and runs one connection's lifetime, driving at most
// one media session at a time (started on "ready", torn down on peer loss).
func connectAndServe(ctx context.Context, url, token string, src frameSource, allowInput bool) error {
	var opts *websocket.DialOptions
	if token != "" {
		// The host authenticates like any other client — no loopback exemption.
		opts = &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}}
	}
	ws, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return err
	}
	defer ws.Close(websocket.StatusNormalClosure, "bye")
	ws.SetReadLimit(1 << 20)
	log.Printf("connected to signaling %s as host", url)

	send := func(v any) error {
		data, _ := json.Marshal(v)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return ws.Write(wctx, websocket.MessageText, data)
	}

	var mu sync.Mutex
	var sess *session

	endSession := func() {
		mu.Lock()
		s := sess
		sess = nil
		mu.Unlock()
		if s != nil {
			s.close()
		}
	}
	defer endSession()

	current := func() *session { mu.Lock(); defer mu.Unlock(); return sess }

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var m sigMsg
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("bad signaling message: %v", err)
			continue
		}
		switch m.Type {
		case "ready":
			endSession() // start fresh for a (re)connecting browser
			s, err := newSession(ctx, src, allowInput, send)
			if err != nil {
				log.Printf("session start failed: %v", err)
				continue
			}
			mu.Lock()
			sess = s
			mu.Unlock()
		case "answer":
			if s := current(); s != nil {
				if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer, SDP: m.SDP,
				}); err != nil {
					log.Printf("set answer: %v", err)
				}
			}
		case "candidate":
			if s := current(); s != nil && len(m.Candidate) > 0 {
				var init webrtc.ICECandidateInit
				if err := json.Unmarshal(m.Candidate, &init); err == nil {
					if err := s.pc.AddICECandidate(init); err != nil {
						log.Printf("add candidate: %v", err)
					}
				}
			}
		case "peer-disconnected":
			log.Printf("browser disconnected; tearing down session")
			endSession()
		}
	}
}

type session struct {
	pc     *webrtc.PeerConnection
	cancel context.CancelFunc
}

func (s *session) close() {
	s.cancel()
	_ = s.pc.Close()
}

// newSession builds the peer connection, attaches the source's video track, and sends
// the offer. The host is the offerer so that in the real product it "publishes" to
// whichever browser connects.
func newSession(parent context.Context, src frameSource, allowInput bool, send func(any) error) (*session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: stun})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	fail := func(e error) (*session, error) { _ = pc.Close(); cancel(); return nil, e }

	// Input data channel: created ONLY when input is enabled and the source can accept
	// it. A view-only session never negotiates this channel, so the browser has nothing
	// to send control to. The host validates every message before forwarding.
	if sink, ok := src.(inputTarget); ok && allowInput {
		dc, derr := pc.CreateDataChannel("input", nil)
		if derr != nil {
			return fail(derr)
		}
		dc.OnOpen(func() { log.Printf("input channel open") })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if line, ok := translateInput(msg.Data); ok {
				sink.writeInput(line)
			}
		})
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(src.codec(), "video", "pion-host")
	if err != nil {
		return fail(err)
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fail(err)
	}
	drainRTCP(videoSender)

	var audioTrack *webrtc.TrackLocalStaticSample
	if src.hasAudio() {
		audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		}, "audio", "pion-host")
		if err != nil {
			return fail(err)
		}
		audioSender, aerr := pc.AddTrack(audioTrack)
		if aerr != nil {
			return fail(aerr)
		}
		drainRTCP(audioSender)
	}

	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("pc state: %s", st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			once.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			cancel()
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		raw, _ := json.Marshal(c.ToJSON())
		_ = send(sigMsg{Type: "candidate", Candidate: raw})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fail(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fail(err)
	}
	if err := send(sigMsg{Type: "offer", SDP: pc.LocalDescription().SDP}); err != nil {
		return fail(err)
	}

	// Wait for the connection, then let the source pump samples into the track.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-connected:
		}
		log.Printf("connected; streaming from source")
		if err := src.stream(ctx, videoTrack, audioTrack); err != nil && ctx.Err() == nil {
			log.Printf("source stream error: %v", err)
		}
	}()

	log.Printf("session started; offer sent")
	return &session{pc: pc, cancel: cancel}, nil
}

// drainRTCP reads and discards a sender's RTCP so interceptors (NACK / receiver
// reports) keep working.
func drainRTCP(sender *webrtc.RTPSender) {
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
}
