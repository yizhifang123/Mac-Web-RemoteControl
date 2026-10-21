package host

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestMoveSlotSumsRelativeAndKeepsLatestAbsolute(t *testing.T) {
	var s moveSlot
	if s.take() != nil {
		t.Fatal("empty slot returned a move")
	}
	s.put(clientInput{T: "r", DX: 3, DY: -1, F: 0})
	s.put(clientInput{T: "r", DX: 4, DY: -2, F: 1})
	if got := string(s.take()); got != "r 7.0 -3.0 1\n" {
		t.Fatalf("relative moves must sum, newest modifiers win: got %q", got)
	}
	if s.take() != nil {
		t.Fatal("take must clear the slot")
	}
	s.put(clientInput{T: "m", X: 0.1, Y: 0.2})
	s.put(clientInput{T: "m", X: 0.5, Y: 0.6})
	if got := string(s.take()); got != "m 0.50000 0.60000 0\n" {
		t.Fatalf("absolute moves: latest must win, got %q", got)
	}
	s.put(clientInput{T: "k", C: "KeyA"})
	if s.take() != nil {
		t.Fatal("a non-move must never enter the move slot")
	}
}

type bufCloser struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *bufCloser) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (*bufCloser) Close() error { return nil }

// A click must never overtake the move that preceded it.
func TestWriteInputFlushesPendingMoveFirst(t *testing.T) {
	out := &bufCloser{}
	s := &screenSource{stdin: out}
	s.writeMove(clientInput{T: "m", X: 0.25, Y: 0.75})
	s.writeInput([]byte("d 0 0.25000 0.75000 0\n"))
	want := "m 0.25000 0.75000 0\nd 0 0.25000 0.75000 0\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWantsKeyframe(t *testing.T) {
	if !wantsKeyframe([]rtcp.Packet{&rtcp.ReceiverReport{}, &rtcp.PictureLossIndication{}}) {
		t.Fatal("PLI must request a keyframe")
	}
	if !wantsKeyframe([]rtcp.Packet{&rtcp.FullIntraRequest{}}) {
		t.Fatal("FIR must request a keyframe")
	}
	if wantsKeyframe([]rtcp.Packet{&rtcp.ReceiverReport{}, &rtcp.TransportLayerNack{}}) {
		t.Fatal("reports and NACKs must not request a keyframe")
	}
}

func TestPlayoutDelayInterceptorStampsNegotiatedStreams(t *testing.T) {
	var got *rtp.Header
	next := interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		got = h
		return 0, nil
	})
	pi := &playoutDelayInterceptor{}

	w := pi.BindLocalStream(&interceptor.StreamInfo{
		RTPHeaderExtensions: []interceptor.RTPHeaderExtension{{URI: playoutDelayURI, ID: 5}},
	}, next)
	if _, err := w.Write(&rtp.Header{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var ext rtp.PlayoutDelayExtension
	if err := ext.Unmarshal(got.GetExtension(5)); err != nil {
		t.Fatalf("extension missing or malformed: %v", err)
	}
	if ext.MinDelay != 0 || ext.MaxDelay != playoutMaxDelay {
		t.Fatalf("got min=%d max=%d", ext.MinDelay, ext.MaxDelay)
	}

	// Not negotiated (audio, or a browser that declined): packets pass untouched.
	w = pi.BindLocalStream(&interceptor.StreamInfo{}, next)
	if _, err := w.Write(&rtp.Header{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got.Extension {
		t.Fatal("un-negotiated stream got an extension")
	}
}

// The extension only reaches the browser if the host's offer advertises it.
func TestOfferAdvertisesPlayoutDelay(t *testing.T) {
	api, err := newAPI()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer.SDP, playoutDelayURI) {
		t.Fatal("offer SDP has no playout-delay extmap")
	}
}

// The move channel is unordered and never retransmits: a lost packet's motion must
// still arrive (running totals), and a late old packet must change nothing.
func TestMoveSeqRecoversLostAndDropsStale(t *testing.T) {
	var q moveSeq
	c := func(n int64, x, y float64) clientInput { return clientInput{T: "c", N: n, CX: x, CY: y} }

	if mv, ok := q.accept(c(1, 10, 0)); !ok || mv.T != "r" || mv.DX != 10 {
		t.Fatalf("first total: got %+v ok=%v", mv, ok)
	}
	// Packet 2 (total 25) is lost; packet 3 carries total 40 → the full 30 arrives.
	if mv, ok := q.accept(c(3, 40, -5)); !ok || mv.DX != 30 || mv.DY != -5 {
		t.Fatalf("after a lost packet: got %+v ok=%v", mv, ok)
	}
	// The lost packet 2 shows up late: stale, must be ignored.
	if _, ok := q.accept(c(2, 25, 0)); ok {
		t.Fatal("a late, older packet was injected")
	}
	// A settle re-send with the same totals injects nothing.
	if _, ok := q.accept(c(4, 40, -5)); ok {
		t.Fatal("unchanged totals produced a move")
	}
	// Absolute moves share the sequence: an old position cannot yank the cursor back.
	if _, ok := q.accept(clientInput{T: "m", N: 4, X: 0.1, Y: 0.1}); ok {
		t.Fatal("stale absolute move accepted")
	}
	if mv, ok := q.accept(clientInput{T: "m", N: 5, X: 0.5, Y: 0.5}); !ok || mv.X != 0.5 {
		t.Fatal("fresh absolute move rejected")
	}
	// Unsequenced moves (reliable-channel fallback) pass straight through.
	if _, ok := q.accept(clientInput{T: "r", DX: 1}); !ok {
		t.Fatal("unsequenced relative move rejected")
	}
}

// "c" exists only on the sequenced path; on its own it renders nothing.
func TestCumulativeMoveNeverRendersDirectly(t *testing.T) {
	if _, ok := translateInput([]byte(`{"t":"c","n":1,"cx":5,"cy":5}`)); ok {
		t.Fatal(`translateInput accepted a bare "c"`)
	}
	if _, ok := decodeInput([]byte(`{"t":"c","n":0,"cx":5}`)); ok {
		t.Fatal(`"c" without a sequence number accepted`)
	}
	if _, ok := decodeInput([]byte(`{"t":"m","n":-1,"x":0.5,"y":0.5}`)); ok {
		t.Fatal("negative sequence number accepted")
	}
}

// A stall can pile up more motion than one injected move may carry; it must leave in
// clamp-sized pieces, not be clamped away.
func TestMoveSlotSplitsOversizedDeltasInsteadOfClamping(t *testing.T) {
	var s moveSlot
	s.put(clientInput{T: "r", DX: 2500, DY: -10})
	var lines []string
	for mv := s.take(); mv != nil; mv = s.take() {
		lines = append(lines, string(mv))
	}
	want := []string{"r 1000.0 -10.0 0\n", "r 1000.0 0.0 0\n", "r 500.0 0.0 0\n"}
	if strings.Join(lines, "") != strings.Join(want, "") {
		t.Fatalf("got %q, want %q", lines, want)
	}
}
