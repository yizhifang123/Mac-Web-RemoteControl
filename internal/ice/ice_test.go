package ice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// cloudflareStub stands in for the Cloudflare TURN API, counting requests so tests
// can see whether the cache actually prevents re-minting.
func cloudflareStub(t *testing.T, status int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("mint used method %s, want POST", r.Method)
		}
		if want := "/v1/turn/keys/key123/credentials/generate-ice-servers"; r.URL.Path != want {
			t.Errorf("mint hit %s, want %s", r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok456" {
			t.Errorf("Authorization = %q; without the bearer token Cloudflare refuses every mint", got)
		}
		payload, _ := io.ReadAll(r.Body)
		var req struct {
			TTL int64 `json:"ttl"`
		}
		if err := json.Unmarshal(payload, &req); err != nil || req.TTL <= 0 {
			t.Errorf("mint body %q: want a positive ttl; without one Cloudflare picks the lifetime", payload)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func testProvider(baseURL string) *Provider {
	return &Provider{
		TurnConfig: TurnConfig{KeyID: "key123", APIToken: "tok456"},
		BaseURL:    baseURL,
	}
}

// mintResponse mirrors the documented 201 payload: a STUN-only entry plus a TURN
// entry carrying credentials.
const mintResponse = `{"iceServers":[
	{"urls":["stun:stun.cloudflare.com:3478"]},
	{"urls":["turn:turn.cloudflare.com:3478?transport=udp","turns:turn.cloudflare.com:443?transport=tcp"],
	 "username":"cf-user","credential":"cf-secret"}
]}`

func TestServersMintsCloudflareCredentials(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusCreated, mintResponse, &hits)
	defer srv.Close()

	servers := testProvider(srv.URL).Servers(context.Background())
	if len(servers) != 2 {
		t.Fatalf("got %d ice servers, want 2 (stun + turn)", len(servers))
	}
	turn := servers[1]
	if turn.Username != "cf-user" {
		t.Errorf("turn username = %q, want cf-user; without it the relay rejects allocation", turn.Username)
	}
	if cred, ok := turn.Credential.(string); !ok || cred != "cf-secret" {
		t.Errorf("turn credential = %#v, want the string \"cf-secret\"; pion expects a string "+
			"for the default password credential type", turn.Credential)
	}
	var hasTLS443 bool
	for _, u := range turn.URLs {
		if strings.HasPrefix(u, "turns:") && strings.Contains(u, ":443") {
			hasTLS443 = true
		}
	}
	if !hasTLS443 {
		t.Error("no turns:…:443 URL survived unmarshalling; that endpoint is the one that " +
			"traverses school firewalls, so losing it defeats the whole feature")
	}
}

func TestServersFallsBackWhenCloudflareErrors(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusForbidden, `{"success":false}`, &hits)
	defer srv.Close()

	servers := testProvider(srv.URL).Servers(context.Background())
	if len(servers) != 1 || len(servers[0].URLs) != 1 || !strings.HasPrefix(servers[0].URLs[0], "stun:") {
		t.Fatalf("got %+v, want the STUN-only fallback; a TURN outage must degrade the "+
			"session to STUN, never fail it", servers)
	}
}

func TestServersCachesUntilNearExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusCreated, mintResponse, &hits)
	defer srv.Close()
	p := testProvider(srv.URL)

	first := p.Servers(context.Background())
	second := p.Servers(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("two Servers calls hit Cloudflare %d times, want 1; every browser "+
			"reconnect would otherwise cost a fresh API round-trip", hits.Load())
	}
	if len(first) != len(second) || second[1].Username != first[1].Username {
		t.Error("cached call returned different servers than the minted ones")
	}

	// Force the cache within the refresh margin: the next call must re-mint so a
	// browser is never handed credentials that expire mid-session.
	p.mu.Lock()
	p.expires = time.Now().Add(refreshMargin / 2)
	p.mu.Unlock()
	p.Servers(context.Background())
	if hits.Load() != 2 {
		t.Errorf("near-expiry call hit Cloudflare %d times total, want 2; stale credentials "+
			"would be handed out until they die mid-session", hits.Load())
	}
}

// After a failed mint the next calls must return immediately from the cooldown
// instead of re-hitting Cloudflare — Servers runs inline in the host's signaling
// read loop and in every /ice-servers request, so retry-per-call would turn each
// browser reconnect into a stall behind the mint timeout.
func TestServersFailureCooldownAvoidsRepeatMints(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusInternalServerError, `upstream sad`, &hits)
	defer srv.Close()
	p := testProvider(srv.URL)

	p.Servers(context.Background())
	servers := p.Servers(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("a call during the failure cooldown hit Cloudflare again (%d hits total, want 1)",
			hits.Load())
	}
	if len(servers) != 1 || !strings.HasPrefix(servers[0].URLs[0], "stun:") {
		t.Errorf("cooldown call returned %+v, want the STUN-only fallback", servers)
	}

	// Once the cooldown passes, minting must resume — the cooldown is a breather,
	// not a permanent downgrade to STUN.
	p.mu.Lock()
	p.cooldown = time.Time{}
	p.mu.Unlock()
	p.Servers(context.Background())
	if hits.Load() != 2 {
		t.Errorf("after the cooldown expired Cloudflare saw %d hits total, want 2; "+
			"a transient outage would disable TURN for the rest of the process lifetime", hits.Load())
	}
}

// Concurrent callers must share ONE upstream mint (single-flight): a burst of
// reconnects racing Cloudflare would waste API calls and, worse, serialize sessions
// behind each other's HTTP round-trips.
func TestServersSingleFlightsConcurrentMints(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold the request open so callers really do overlap
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(mintResponse))
	}))
	defer srv.Close()
	p := testProvider(srv.URL)

	const callers = 8
	results := make(chan []webrtc.ICEServer, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- p.Servers(context.Background()) }()
	}
	time.Sleep(50 * time.Millisecond) // let the callers pile up behind the flight
	close(release)

	for i := 0; i < callers; i++ {
		if servers := <-results; len(servers) != 2 || servers[1].Username != "cf-user" {
			t.Errorf("a concurrent caller got %+v, want the minted servers", servers)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("%d concurrent calls made %d upstream requests, want 1", callers, hits.Load())
	}
}

func TestServersWithoutKeyNeverCallsCloudflare(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusCreated, mintResponse, &hits)
	defer srv.Close()

	for _, tc := range []struct {
		name string
		p    *Provider
	}{
		{"nil provider", nil},
		{"zero provider", &Provider{BaseURL: srv.URL}},
		{"key id only", &Provider{TurnConfig: TurnConfig{KeyID: "key123"}, BaseURL: srv.URL}},
		{"token only", &Provider{TurnConfig: TurnConfig{APIToken: "tok456"}, BaseURL: srv.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers := tc.p.Servers(context.Background())
			if len(servers) != 1 || !strings.HasPrefix(servers[0].URLs[0], "stun:") {
				t.Errorf("got %+v, want the STUN-only fallback", servers)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("an unconfigured provider called Cloudflare %d times; without a key there "+
			"is nothing valid to send, only noise and latency", hits.Load())
	}
}

// Fallback must return a fresh slice: a caller (pion) holding the returned value
// must not be able to corrupt what later callers receive.
func TestFallbackIsNotShared(t *testing.T) {
	a := Fallback()
	a[0].URLs[0] = "stun:evil.example.com:1"
	if b := Fallback(); b[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Error("mutating one Fallback() result corrupted the next; the fallback must be fresh per call")
	}
}

// The contract this package relies on: Cloudflare's iceServers entries decode
// directly into webrtc.ICEServer and pion accepts them — otherwise the host side
// would fail at NewPeerConnection on every session start.
func TestMintedServersAreAcceptedByPion(t *testing.T) {
	var hits atomic.Int32
	srv := cloudflareStub(t, http.StatusCreated, mintResponse, &hits)
	defer srv.Close()

	servers := testProvider(srv.URL).Servers(context.Background())
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		t.Fatalf("pion rejects the minted servers: %v; every session start would fail", err)
	}
	_ = pc.Close()
}
