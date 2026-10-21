package host

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/signaling"
)

// startStack brings up a signaling server and a host on loopback, standing in for
// `./dev.sh run`. The synthetic VP8 file source is used deliberately: no screen
// capture, no audio, no TCC prompts.
func startStack(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = signaling.Serve(ctx, ln, fstest.MapFS{}, nil, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"
	go func() {
		_ = Run(ctx, Config{
			SignalURL: url,
			Source:    "test",
			MediaPath: "../../media/testsrc.ivf",
		})
	}()
	return url
}

// browser is a pion peer standing in for the client page: it drives the same
// signaling protocol web/index.html speaks.
type browser struct {
	ws        *websocket.Conn
	pc        *webrtc.PeerConnection
	connected chan struct{}
	gotMedia  chan struct{}
}

func dialBrowser(t *testing.T, url string) *browser {
	t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, url+"?role=client", nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	ws.SetReadLimit(1 << 20)

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	b := &browser{ws: ws, pc: pc, connected: make(chan struct{}), gotMedia: make(chan struct{})}

	var closeConnected, closeMedia bool
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected && !closeConnected {
			closeConnected = true
			close(b.connected)
		}
	})
	pc.OnTrack(func(*webrtc.TrackRemote, *webrtc.RTPReceiver) {
		if !closeMedia {
			closeMedia = true
			close(b.gotMedia)
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		raw, _ := json.Marshal(c.ToJSON())
		msg, _ := json.Marshal(sigMsg{Type: "candidate", Candidate: raw})
		_ = ws.Write(ctx, websocket.MessageText, msg)
	})

	go b.readLoop(t)

	// The client asks for the stream — the server no longer volunteers a "ready".
	ready, _ := json.Marshal(sigMsg{Type: "ready"})
	if err := ws.Write(ctx, websocket.MessageText, ready); err != nil {
		t.Fatalf("send ready: %v", err)
	}
	return b
}

func (b *browser) readLoop(t *testing.T) {
	ctx := context.Background()
	for {
		_, data, err := b.ws.Read(ctx)
		if err != nil {
			return // socket gone; the point of several of these tests
		}
		var m sigMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "offer":
			if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer, SDP: m.SDP,
			}); err != nil {
				return
			}
			answer, err := b.pc.CreateAnswer(nil)
			if err != nil {
				return
			}
			if err := b.pc.SetLocalDescription(answer); err != nil {
				return
			}
			out, _ := json.Marshal(sigMsg{Type: "answer", SDP: b.pc.LocalDescription().SDP})
			_ = b.ws.Write(ctx, websocket.MessageText, out)
		case "candidate":
			if len(m.Candidate) > 0 {
				var init webrtc.ICECandidateInit
				if json.Unmarshal(m.Candidate, &init) == nil {
					_ = b.pc.AddICECandidate(init)
				}
			}
		}
	}
}

func (b *browser) awaitConnected(t *testing.T) {
	t.Helper()
	select {
	case <-b.connected:
	case <-time.After(20 * time.Second):
		t.Fatalf("peer connection never reached connected (state %s)", b.pc.ConnectionState())
	}
	select {
	case <-b.gotMedia:
	case <-time.After(10 * time.Second):
		t.Fatal("no media track arrived")
	}
}

// This is the ~5-minute-disconnect regression. The signaling socket carries nothing
// once ICE completes, so a firewall or tunnel reconnect can reap it at any moment.
// Media flows peer-to-peer and is entirely unaffected — the host used to close the
// peer connection anyway, killing a working stream and forcing a page refresh.
func TestSignalingDropLeavesMediaAlive(t *testing.T) {
	url := startStack(t)
	b := dialBrowser(t, url)
	defer b.pc.Close()
	b.awaitConnected(t)

	// Reap the signaling socket exactly as an idle-timeout would: abruptly, with no
	// close handshake, while media is healthy.
	_ = b.ws.CloseNow()

	// The host's teardown was immediate, so a short wait is a real signal; keep it
	// long enough to also cover its 2s signaling redial.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if st := b.pc.ConnectionState(); st != webrtc.PeerConnectionStateConnected {
			t.Fatalf("media died %v after the signaling socket dropped (state %s); "+
				"a signaling drop must never tear down a working stream",
				time.Until(deadline), st)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// A browser whose socket was reaped reconnects rather than making the user refresh.
// The stale peer must not hold the "client" role against its own replacement.
func TestReconnectAfterSignalingDropGetsAFreshStream(t *testing.T) {
	url := startStack(t)
	b := dialBrowser(t, url)
	b.awaitConnected(t)
	_ = b.ws.CloseNow()
	_ = b.pc.Close() // this browser is gone for good; a new page load follows

	b2 := dialBrowser(t, url)
	defer b2.pc.Close()
	b2.awaitConnected(t) // fails if the evicted peer still owns the role
}
