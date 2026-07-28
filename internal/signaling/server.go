// Package signaling relays WebRTC SDP and ICE between exactly two peers — the Mac
// host and one browser — and serves the static client page.
//
// It is meant to bind 127.0.0.1 ONLY, reached exclusively through a tunnel with the
// auth gate in front of it, never exposed directly. See docs/SECURITY.md.
package signaling

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
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

// add registers p under role, returning whether both roles are now present.
func (h *hub) add(role string, p *peer) (bothPresent bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.peers[role]; taken {
		return false, errors.New("role already connected")
	}
	h.peers[role] = p
	_, other := h.peers[otherRole(role)]
	return other, nil
}

func (h *hub) remove(role string, p *peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peers[role] == p {
		delete(h.peers, role)
	}
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

	both, err := h.add(role, p)
	if err != nil {
		log.Printf("reject %s: %v", role, err)
		_ = c.Close(websocket.StatusPolicyViolation, "role already connected")
		return
	}
	log.Printf("%s connected (bothPresent=%v)", role, both)
	defer func() {
		h.remove(role, p)
		if o := h.get(otherRole(role)); o != nil {
			_ = o.send(context.Background(), []byte(`{"type":"peer-disconnected"}`))
		}
		_ = c.Close(websocket.StatusNormalClosure, "bye")
		log.Printf("%s disconnected", role)
	}()

	// Once both peers are present, tell the host to make the offer.
	if both {
		if host := h.get("host"); host != nil {
			_ = host.send(context.Background(), []byte(`{"type":"ready"}`))
		}
	}

	ctx := r.Context()
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

// Handler returns the signaling WebSocket endpoint plus the static client page,
// served from web (embedded assets in a built binary, or a directory in dev).
//
// gate must be non-nil in any real deployment: it is the only thing standing between
// the network and a channel that injects keystrokes. A nil gate serves everything
// unauthenticated and is accepted only for loopback dev runs (see cmd/play -no-auth).
func Handler(web fs.FS, gate *auth.Auth) (http.Handler, error) {
	h := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.handleWS)
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

// Serve runs the signaling server on ln until ctx is cancelled, then shuts it down.
// Callers bind the listener themselves so an in-process host (cmd/play) knows the
// server is accepting before it dials.
func Serve(ctx context.Context, ln net.Listener, web fs.FS, gate *auth.Auth) error {
	handler, err := Handler(web, gate)
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
