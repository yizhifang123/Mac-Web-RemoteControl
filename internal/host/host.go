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
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/ice"
)

// sigMsg is the tiny signaling protocol. Offer/answer carry SDP; candidate carries a
// raw ICECandidateInit forwarded verbatim by the server. Message rides on
// type:"error", the host telling the viewer why its session just ended (the server
// relays it to the browser like everything else).
type sigMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	Message   string          `json:"message,omitempty"`
}

// Config is everything the host needs to run its session loop.
type Config struct {
	SignalURL   string
	Token       string // bearer token for the signaling gate (empty only when auth is off)
	Source      string // screen | screen-test | test
	MediaPath   string // VP8 IVF file, source=test only
	CapturePath string // Swift capture helper, source=screen*

	// ICEServers supplies the servers for each new peer connection — TURN
	// credentials when cmd/play has a Cloudflare TURN key configured. Nil means
	// the built-in STUN-only default. Like SignalURL this is set by the cmd, not a
	// flag: cmd/play wires in its own minting provider, cmd/host stays STUN-only.
	ICEServers func(context.Context) []webrtc.ICEServer

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
	width := fs.Int("width", 1920, "capture width (height follows the display's aspect)")
	height := fs.Int("height", 1080, "capture height (overridden to match display aspect on source=screen)")
	fps := fs.Int("fps", 30, "capture frame rate")
	// Scaled with the 1280→1920 default: 8 Mbps was tuned for 720p-class frames and
	// leaves 1080p-class ones soft on motion and small text, which is most of what
	// this gets used for.
	bitrate := fs.Int("bitrate", 12_000_000, "encoder bitrate (bits/sec)")
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

	// The session holder lives out here, OUTSIDE the reconnect loop, on purpose: a
	// media session must survive losing the signaling socket. See sessionHolder.
	var sessions sessionHolder
	defer sessions.end()

	for {
		if err := connectAndServe(ctx, hostURL, cfg.Token, src, enableInput, cfg.ICEServers, &sessions); err != nil {
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

// connectAndServe dials signaling and runs one connection's lifetime. The media
// session it drives is owned by sessions, which outlives this call: returning from
// here means the signaling socket died, and that is not a reason to stop streaming.
func connectAndServe(ctx context.Context, url, token string, src frameSource, allowInput bool, iceServers func(context.Context) []webrtc.ICEServer, sessions *sessionHolder) error {
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

	// NOTE: deliberately no `defer sessions.end()`. Losing signaling must leave a
	// working stream alone — the browser keeps its video while this loop redials.

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
			// The browser asked for a stream. End any current one FIRST — the source
			// can only feed one session at a time — then build the replacement.
			sessions.end()
			s, err := newSession(ctx, src, allowInput, send, iceServers, sessions)
			if err != nil {
				log.Printf("session start failed: %v", err)
				continue
			}
			sessions.set(s)
		case "answer":
			if s := sessions.get(); s != nil {
				if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer, SDP: m.SDP,
				}); err != nil {
					log.Printf("set answer: %v", err)
				}
			}
		case "candidate":
			if s := sessions.get(); s != nil && len(m.Candidate) > 0 {
				var init webrtc.ICECandidateInit
				if err := json.Unmarshal(m.Candidate, &init); err == nil {
					if err := s.pc.AddICECandidate(init); err != nil {
						log.Printf("add candidate: %v", err)
					}
				}
			}
		case "peer-disconnected":
			// The browser's SIGNALING socket went away. That says nothing about the
			// media path, which is peer-to-peer (or relayed) and never touches this
			// connection. Tearing down here is what killed a healthy stream every
			// time an idle socket got reaped. If the browser is really gone, the peer
			// connection fails on its own and the handler below cleans up; if it just
			// reconnects, it keeps the video it already had.
			log.Printf("browser signaling disconnected; keeping the media session")
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

// sessionHolder owns the at-most-one live media session, independently of any single
// signaling connection. Media flows peer-to-peer or through TURN and does not touch
// the signaling socket, so a signaling drop must never close it — conflating the two
// was the ~5-minute disconnect.
type sessionHolder struct {
	mu   sync.Mutex
	sess *session
}

func (h *sessionHolder) get() *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess
}

// set installs s and closes whatever it replaced.
func (h *sessionHolder) set(s *session) {
	h.mu.Lock()
	old := h.sess
	h.sess = s
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (h *sessionHolder) end() { h.set(nil) }

// endIf closes s only if it is still the current session, so a late callback from a
// peer connection that has already been replaced cannot kill its successor.
func (h *sessionHolder) endIf(s *session) {
	h.mu.Lock()
	current := h.sess == s
	if current {
		h.sess = nil
	}
	h.mu.Unlock()
	if current {
		s.close()
	}
}

// newSession builds the peer connection, attaches the source's video track, and sends
// the offer. The host is the offerer so that in the real product it "publishes" to
// whichever browser connects.
func newSession(parent context.Context, src frameSource, allowInput bool, send func(any) error, iceServers func(context.Context) []webrtc.ICEServer, sessions *sessionHolder) (*session, error) {
	// Sessions come and go with browser reconnects, so the servers are fetched per
	// session — the provider behind iceServers caches, so this is normally a lookup,
	// not an HTTP round-trip.
	servers := ice.Fallback()
	if iceServers != nil {
		servers = iceServers(parent)
	}
	api, err := newAPI()
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	sess := &session{pc: pc, cancel: cancel}
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
		var seq moveSeq
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			// Sequenced moves (the browser's settle/pre-click re-send) share the move
			// channel's sequence; everything else is a plain validated command.
			if m, ok := decodeInput(msg.Data); ok && m.N > 0 {
				if mv, fresh := seq.accept(m); fresh {
					sink.writeMove(mv)
				}
				return
			}
			if line, ok := translateInput(msg.Data); ok {
				sink.writeInput(line)
			}
		})

		// Mouse moves get their own channel: unordered, never retransmitted. On the
		// reliable channel one lost packet holds every later move (and key) until it
		// is resent, then replays them all late. Here a lost move costs nothing: the
		// next one carries the latest position, or the running totals (see moveSeq).
		// Keys, clicks and wheel stay on "input", where losing one would be a bug.
		unordered, noRetransmits := false, uint16(0)
		mc, merr := pc.CreateDataChannel("move", &webrtc.DataChannelInit{
			Ordered: &unordered, MaxRetransmits: &noRetransmits,
		})
		if merr != nil {
			return fail(merr)
		}
		mc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if m, ok := decodeInput(msg.Data); ok && (m.T == "m" || m.T == "c") {
				if mv, fresh := seq.accept(m); fresh {
					sink.writeMove(mv)
				}
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
	kf, _ := src.(keyframer)
	readRTCPForKeyframes(videoSender, kf)

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
			logSelectedPath(pc)
		case webrtc.PeerConnectionStateFailed:
			// THIS is what ends a session: the media path itself is gone. (Note
			// Disconnected is deliberately not handled — it is transient, and ICE
			// recovers from it on its own.) Closing a peer connection from inside a
			// pion state callback deadlocks, so hand it off to a goroutine.
			cancel()
			go sessions.endIf(sess)
		case webrtc.PeerConnectionStateClosed:
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
			// The source died under a live peer connection. Left alone, that is the
			// black-screen session: connected, silent, showing nothing. Tell the
			// viewer why (best-effort — the signaling socket may be mid-reconnect),
			// then end the session so the browser's retry loop asks for a fresh one.
			log.Printf("source stream error: %v", err)
			_ = send(sigMsg{Type: "error", Message: err.Error()})
			sessions.endIf(sess)
		}
	}()

	log.Printf("session started; offer sent")
	return sess, nil
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
