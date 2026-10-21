// Package signaling relays WebRTC SDP and ICE between exactly two peers — the Mac
// host and one browser — and serves the static client page.
//
// It is meant to bind 127.0.0.1 ONLY, reached exclusively through a tunnel with the
// auth gate in front of it, never exposed directly. See docs/SECURITY.md.
package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/ice"
)

const (
	// pingInterval / pingTimeout keep the signaling socket from ever looking idle.
	// Once ICE completes nothing else is sent on it for the rest of the call, and an
	// idle connection crossing a school firewall, the Cloudflare edge and a cloudflared
	// tunnel gets reaped within minutes — which used to take the media down with it
	// (the ~5-minute disconnect). Pings also surface a
	// half-dead peer promptly instead of leaving its role held by a corpse.
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
)

// peer is one side of the session. A mutex serializes writes because
// coder/websocket permits only one concurrent writer per connection.
type peer struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (p *peer) send(ctx context.Context, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.ws.Write(ctx, websocket.MessageText, data)
}

// hub holds the single host+client pair. Phase 1 supports exactly one session; a
// second connection for an already-filled role is rejected.
type hub struct {
	mu    sync.Mutex
	peers map[string]*peer
}

func newHub() *hub { return &hub{peers: map[string]*peer{}} }

// add registers p under role, returning any earlier connection it displaced.
//
// A role already held is EVICTED, not refused. This is a single-user system, and the
// realistic cause of a collision is a peer reconnecting before the server noticed its
// old socket had died — refusing locked the newcomer out until the corpse timed out,
// turning a recoverable blip into a dead session. Newest connection wins.
func (h *hub) add(role string, p *peer) (displaced *peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.peers[role]
	h.peers[role] = p
	return old
}

// remove drops p from role, reporting whether it was still the registered peer.
// A false return means p had already been displaced by a reconnect, in which case
// its departure is not news the other side should hear about.
func (h *hub) remove(role string, p *peer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peers[role] == p {
		delete(h.peers, role)
		return true
	}
	return false
}

func (h *hub) get(role string) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers[role]
}

func otherRole(role string) string {
	if role == "host" {
		return "client"
	}
	return "host"
}

func (h *hub) handleWS(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role != "host" && role != "client" {
		http.Error(w, "role must be host or client", http.StatusBadRequest)
		return
	}

	c, err := websocket.Accept(w, r, nil) // same-origin browser + non-browser dialer both pass
	if err != nil {
		log.Printf("ws accept (%s): %v", role, err)
		return
	}
	c.SetReadLimit(1 << 20) // SDP can be a few KB; be generous
	p := &peer{ws: c}

	if old := h.add(role, p); old != nil {
		log.Printf("%s reconnected; evicting the stale connection", role)
		_ = old.ws.CloseNow()
	}
	log.Printf("%s connected", role)
	defer func() {
		// Only announce a departure that actually vacated the role. A peer we
		// already evicted never left as far as the other side is concerned.
		if h.remove(role, p) {
			if o := h.get(otherRole(role)); o != nil {
				_ = o.send(context.Background(), []byte(`{"type":"peer-disconnected"}`))
			}
		}
		_ = c.Close(websocket.StatusNormalClosure, "bye")
		log.Printf("%s disconnected", role)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go keepalive(ctx, c, role)

	// Nothing is announced when both peers are present: the CLIENT asks for an offer
	// (a relayed {"type":"ready"}) when it actually needs one. That ordering matters —
	// a server-driven "ready" fires on every reconnect and would rebuild a peer
	// connection that is still happily streaming, which is exactly what this whole
	// change exists to stop. The client knows whether its media is alive; the server
	// does not, so the server stays a dumb relay.
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return // normal close or peer gone
		}
		if o := h.get(otherRole(role)); o != nil {
			if err := o.send(ctx, data); err != nil {
				log.Printf("forward %s: %v", role, err)
			}
		}
	}
}

// keepalive pings c every pingInterval until ctx ends. A ping that goes unanswered
// means the peer is gone even though the socket still looks open, so the connection
// is closed hard to unblock the read loop and run the disconnect path.
//
// Browsers answer ping control frames automatically, so this needs no client support.
func keepalive(ctx context.Context, c *websocket.Conn, role string) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("%s keepalive failed: %v", role, err)
					_ = c.CloseNow()
				}
				return
			}
		}
	}
}

// Handler returns the signaling WebSocket endpoint plus the static client page,
// served from web (embedded assets in a built binary, or a directory in dev).
//
// gate must be non-nil in any real deployment: it is the only thing standing between
// the network and a channel that injects keystrokes. A nil gate serves everything
// unauthenticated and is accepted only for loopback dev runs (see cmd/play -no-auth).
//
// iceServers supplies /ice-servers, the endpoint the browser fetches its ICE config
// from (TURN credentials when minting is configured). Nil is safe and means the
// STUN-only fallback — cmd/signal runs that way.
func Handler(web fs.FS, gate *auth.Auth, iceServers func(context.Context) []webrtc.ICEServer) (http.Handler, error) {
	h := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.handleWS)
	mux.HandleFunc("/ice-servers", serveICEServers(iceServers))
	mux.Handle("/", http.FileServer(http.FS(web)))
	if gate == nil {
		return mux, nil
	}
	loginPage, err := fs.ReadFile(web, "login.html")
	if err != nil {
		return nil, fmt.Errorf("read login page: %w", err)
	}
	return gate.Protect(mux, loginPage), nil
}

// serveICEServers hands the browser its ICE configuration. In a gated deployment it
// sits behind auth.Protect like everything else on the mux — deliberately: minted
// TURN credentials are metered relay bandwidth, not something to hand every
// anonymous prober of the tunnel hostname.
func serveICEServers(iceServers func(context.Context) []webrtc.ICEServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		servers := ice.Fallback()
		if iceServers != nil {
			servers = iceServers(r.Context())
		}
		body, err := json.Marshal(struct {
			ICEServers []webrtc.ICEServer `json:"iceServers"`
		}{servers})
		if err != nil {
			http.Error(w, "encode ice servers", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Credentials are short-lived; a cached copy would outlive its validity.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

// Serve runs the signaling server on ln until ctx is cancelled, then shuts it down.
// Callers bind the listener themselves so an in-process host (cmd/play) knows the
// server is accepting before it dials.
func Serve(ctx context.Context, ln net.Listener, web fs.FS, gate *auth.Auth, iceServers func(context.Context) []webrtc.ICEServer) error {
	handler, err := Handler(web, gate, iceServers)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return ctx.Err()
	}
}

// LogBindHints prints the LAN URLs to open from a SECOND device (phone / laptop) when the
// server is bound to a wildcard address for local cross-device testing. Testing remote
// control against the same Mac you're typing on is unworkable (mirror + input fighting),
// so drive it from another device on the same network.
//
// SECURITY: a wildcard bind exposes this UNAUTHENTICATED server to the whole LAN — with
// -allow-input, anyone on the network could control the Mac. Use it only briefly on a
// trusted home network, then go back to the default 127.0.0.1 bind. Real remote access
// goes through the tunnel + auth (Phase 6), never a wildcard bind.
func LogBindHints(addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host != "0.0.0.0" && host != "::" && host != "" {
		log.Printf("(loopback only — bind -addr 0.0.0.0:%s to test from another device)", port)
		return
	}
	log.Printf("!! WILDCARD BIND: reachable by any device on this LAN (unauthenticated). Revert to 127.0.0.1 after testing.")
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				log.Printf("   open from another device: http://%s:%s", ip4, port)
			}
		}
	}
}
