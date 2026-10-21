package host

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type countingKF struct{ n atomic.Int32 }

func (c *countingKF) requestKeyframe() { c.n.Add(1) }

// End to end through two real pion peers: the host API keeps pion's default codecs and
// interceptors, video packets that reach the browser side carry playout-delay
// min=0/max=100ms under the NEGOTIATED id, and a PLI burst yields one throttled
// keyframe request.
func TestReviewPlayoutDelayAndPLIEndToEnd(t *testing.T) {
	api, err := newAPI()
	if err != nil {
		t.Fatal(err)
	}
	host, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	// Register some other extensions first so the browser side's ids differ from a
	// naive guess, as Chrome's do.
	for _, u := range []string{"urn:ietf:params:rtp-hdrext:toffset", "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"} {
		_ = me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: u}, webrtc.RTPCodecTypeVideo)
	}
	if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: playoutDelayURI}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		t.Fatal(err)
	}
	browser, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion-host")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := host.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	kf := &countingKF{}
	readRTCPForKeyframes(sender, kf)

	type got struct {
		pkt *rtp.Packet
		id  int
	}
	pktCh := make(chan got, 1)
	browser.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		id := 0
		for _, e := range rcv.GetParameters().HeaderExtensions {
			if e.URI == playoutDelayURI {
				id = e.ID
			}
		}
		for i := 0; i < 2; i++ { // PLI burst: must collapse to one keyframe request
			_ = browser.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}})
		}
		p, _, err := tr.ReadRTP()
		if err == nil {
			select {
			case pktCh <- got{p, id}:
			default:
			}
		}
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
		}
	})

	offer, err := host.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"VP8", "H264", "nack", "transport-cc", playoutDelayURI} {
		if !strings.Contains(offer.SDP, want) {
			t.Errorf("offer lost pion default %q", want)
		}
	}
	gh := webrtc.GatheringCompletePromise(host)
	if err := host.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gh
	if err := browser.SetRemoteDescription(*host.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	ans, err := browser.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gb := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(ans); err != nil {
		t.Fatal(err)
	}
	<-gb
	if err := host.SetRemoteDescription(*browser.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	nal := append([]byte{0, 0, 0, 1, 0x65}, make([]byte, 200)...)
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var g got
wait:
	for {
		select {
		case g = <-pktCh:
			break wait
		case <-deadline:
			t.Fatal("no RTP reached the browser peer")
		case <-tick.C:
			_ = track.WriteSample(media.Sample{Data: nal, Duration: 33 * time.Millisecond})
		}
	}
	if g.id == 0 {
		t.Fatal("playout-delay not negotiated on the video receiver")
	}
	var ext rtp.PlayoutDelayExtension
	if err := ext.Unmarshal(g.pkt.Header.GetExtension(uint8(g.id))); err != nil {
		t.Fatalf("video packet lacks playout-delay at negotiated id %d: %v (ids present %v)", g.id, err, g.pkt.Header.GetExtensionIDs())
	}
	if ext.MinDelay != 0 || ext.MaxDelay != 10 {
		t.Fatalf("playout-delay min=%d max=%d, want 0/10", ext.MinDelay, ext.MaxDelay)
	}
	waitFor(t, "keyframe request from PLI", func() bool { return kf.n.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := kf.n.Load(); n != 1 {
		t.Fatalf("PLI burst produced %d keyframe requests, want 1 (throttle)", n)
	}
}
