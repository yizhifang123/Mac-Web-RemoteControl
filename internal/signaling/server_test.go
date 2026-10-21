package signaling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pion/webrtc/v4"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
)

// testWeb is the minimal client bundle Handler needs: the gated path reads
// login.html to render its own gate.
var testWeb = fstest.MapFS{
	"index.html": &fstest.MapFile{Data: []byte("CLIENT PAGE")},
	"login.html": &fstest.MapFile{Data: []byte("LOGIN PAGE")},
}

// stubServers is a canned ICE fetcher standing in for the Cloudflare-minting
// provider, distinctive enough that a response can't be confused with the fallback.
func stubServers(context.Context) []webrtc.ICEServer {
	return []webrtc.ICEServer{{
		URLs:       []string{"turn:turn.example.com:3478?transport=udp", "turns:turn.example.com:443?transport=tcp"},
		Username:   "stub-user",
		Credential: "stub-secret",
	}}
}

func decodeICEResponse(t *testing.T, body string) []webrtc.ICEServer {
	t.Helper()
	var out struct {
		ICEServers []webrtc.ICEServer `json:"iceServers"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not the {\"iceServers\":[…]} shape the client parses: %v (body %q)", err, body)
	}
	return out.ICEServers
}

func TestICEServersEndpointUngated(t *testing.T) {
	h, err := Handler(testWeb, nil, stubServers)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ice-servers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /ice-servers = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	servers := decodeICEResponse(t, w.Body.String())
	if len(servers) != 1 || servers[0].Username != "stub-user" {
		t.Errorf("got %+v, want the stub fetcher's TURN entry; the browser would connect "+
			"STUN-only and stall on restrictive networks", servers)
	}
	if cred, ok := servers[0].Credential.(string); !ok || cred != "stub-secret" {
		t.Errorf("credential = %#v, want the string \"stub-secret\"; without it the "+
			"browser cannot allocate on the relay", servers[0].Credential)
	}
}

func TestICEServersEndpointRejectsNonGET(t *testing.T) {
	h, err := Handler(testWeb, nil, stubServers)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/ice-servers", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /ice-servers = %d, want 405", method, w.Code)
		}
	}
}

// A nil fetcher (cmd/signal) must still serve a usable STUN-only answer — the client
// unconditionally fetches this endpoint, so 404ing it would break the debug server.
func TestICEServersEndpointNilFetcherServesFallback(t *testing.T) {
	h, err := Handler(testWeb, nil, nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ice-servers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /ice-servers = %d, want 200", w.Code)
	}
	servers := decodeICEResponse(t, w.Body.String())
	if len(servers) != 1 || len(servers[0].URLs) != 1 || !strings.HasPrefix(servers[0].URLs[0], "stun:") {
		t.Errorf("got %+v, want the STUN-only fallback", servers)
	}
}

// TURN credentials are metered relay bandwidth: behind a gate, an anonymous request
// must get the login redirect, never the credentials.
func TestICEServersEndpointSitsBehindTheGate(t *testing.T) {
	gate, _, err := auth.Load(filepath.Join(t.TempDir(), "config.json"), false)
	if err != nil {
		t.Fatalf("auth.Load: %v", err)
	}
	h, err := Handler(testWeb, gate, stubServers)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	t.Run("unauthenticated is redirected to login", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ice-servers", nil))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Errorf("redirects to %q, want the login page (optionally carrying ?next=)", loc)
		}
		if strings.Contains(w.Body.String(), "stub-secret") {
			t.Error("TURN credential leaked to an unauthenticated request; anyone probing " +
				"the tunnel hostname could burn metered relay bandwidth")
		}
	})

	t.Run("bearer token gets the credentials", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ice-servers", nil)
		r.Header.Set("Authorization", "Bearer "+gate.Token())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		servers := decodeICEResponse(t, w.Body.String())
		if len(servers) != 1 || servers[0].Username != "stub-user" {
			t.Errorf("got %+v, want the stub fetcher's TURN entry", servers)
		}
	})
}
