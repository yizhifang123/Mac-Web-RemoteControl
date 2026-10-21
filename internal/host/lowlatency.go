package host

import (
	"log"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// playoutDelayURI is the RTP header extension that tells the receiver how long it may
// hold a frame before showing it. Chrome's WebRTC switches to its low-latency
// renderer — render each frame as soon as it is decoded, no smoothing buffer — when
// min == 0 and max <= 500 ms (modules/video_coding/timing/timing.cc,
// UseLowLatencyRendering). That smoothing buffer is tuned for watching video, and is
// exactly what makes a pointer-locked game feel late.
const playoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// playoutMaxDelay is the extension's max, in its 10 ms units: 100 ms. Comfortably
// inside the 500 ms low-latency threshold, but not 0, so a late frame can still be
// composited instead of skipped outright.
const playoutMaxDelay = 10

// newAPI builds pion's default stack (codecs + NACK/reports/TWCC interceptors — what
// webrtc.NewPeerConnection would use) plus the playout-delay extension on video.
func newAPI() (*webrtc.API, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	if err := me.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: playoutDelayURI},
		webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverDirectionSendonly,
	); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, err
	}
	ir.Add(playoutDelayFactory{})
	return webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir)), nil
}

type playoutDelayFactory struct{}

func (playoutDelayFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &playoutDelayInterceptor{}, nil
}

// playoutDelayInterceptor stamps min=0/max=playoutMaxDelay on every packet of any
// local stream that negotiated the extension. Nothing in pion writes it, and
// TrackLocalStaticSample has no per-packet hook, so it lives here. Stamping every
// packet (3 bytes) rather than only until acknowledged keeps it stateless.
type playoutDelayInterceptor struct{ interceptor.NoOp }

func (*playoutDelayInterceptor) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	id := 0
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == playoutDelayURI {
			id = e.ID
		}
	}
	if id == 0 { // not negotiated (audio, or a browser that declined it)
		return w
	}
	ext, err := rtp.PlayoutDelayExtension{MinDelay: 0, MaxDelay: playoutMaxDelay}.Marshal()
	if err != nil {
		return w
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if err := h.SetExtension(uint8(id), ext); err != nil {
			return 0, err
		}
		return w.Write(h, payload, a)
	})
}

// keyframer is a source whose encoder can be asked for an immediate keyframe.
type keyframer interface {
	requestKeyframe()
}

// keyframeMinGap throttles keyframe requests: a burst of loss makes the browser send
// PLI after PLI, and each keyframe is several times a normal frame — honoring every
// one would add to the congestion that caused the loss.
const keyframeMinGap = 300 * time.Millisecond

// readRTCPForKeyframes replaces the plain drain for the video sender. It must keep
// reading forever regardless — the interceptors (NACK responder, reports) do their
// work inside this read — and on PLI/FIR asks the encoder for a keyframe, so a lost
// packet costs one frame instead of a broken picture until the next periodic IDR.
//
// The request itself runs on its own goroutine: it writes to the helper's stdin, which
// can block if the helper stalls, and this read loop must never block.
func readRTCPForKeyframes(sender *webrtc.RTPSender, kf keyframer) {
	kick := make(chan struct{}, 1)
	if kf != nil {
		go func() {
			for range kick {
				kf.requestKeyframe()
			}
		}()
	}
	go func() {
		defer close(kick)
		var last time.Time
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			if kf == nil || !wantsKeyframe(pkts) || time.Since(last) < keyframeMinGap {
				continue
			}
			last = time.Now()
			select {
			case kick <- struct{}{}:
			default: // one already pending — that keyframe covers this request too
			}
		}
	}()
}

func wantsKeyframe(pkts []rtcp.Packet) bool {
	for _, p := range pkts {
		switch p.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			return true
		}
	}
	return false
}

// logSelectedPath logs which network path media actually took — direct (host/srflx)
// or TURN relay, and over UDP or TCP. Without it there is no telling a slow relay
// session from a direct one after the fact. Runs off the callback goroutine:
// GetStats from inside a pion state callback can deadlock.
func logSelectedPath(pc *webrtc.PeerConnection) {
	go func() {
		report := pc.GetStats()
		for _, s := range report {
			pair, ok := s.(webrtc.ICECandidatePairStats)
			if !ok || !pair.Nominated || pair.State != webrtc.StatsICECandidatePairStateSucceeded {
				continue
			}
			local, _ := report[pair.LocalCandidateID].(webrtc.ICECandidateStats)
			remote, _ := report[pair.RemoteCandidateID].(webrtc.ICECandidateStats)
			log.Printf("media path: local %s/%s%s ↔ remote %s/%s, rtt %.0f ms",
				local.CandidateType, local.Protocol, relayVia(local.RelayProtocol),
				remote.CandidateType, remote.Protocol, pair.CurrentRoundTripTime*1000)
			return
		}
		log.Printf("media path: no nominated candidate pair in stats yet")
	}()
}

func relayVia(p string) string {
	if p == "" {
		return ""
	}
	return " via " + p
}
