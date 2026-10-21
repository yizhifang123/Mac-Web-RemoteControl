package signaling

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

func testServer(t *testing.T) string {
	t.Helper()
	h, err := Handler(fstest.MapFS{}, nil, nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):] + "/ws"
}

func dial(t *testing.T, url, role string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), url+"?role="+role, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", role, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// pump reads c on a background context forever, delivering message types on the
// returned channel and closing it when the connection dies.
//
// Reading with an expiring context is NOT usable here: coder/websocket tears the
// connection down when a read context is cancelled, so the obvious "did anything
// arrive in 750ms?" helper destroys the very socket the test goes on to use. Keeping
// a reader parked here also means ping frames get answered, which is what the
// browser does and what the keepalive assumes.
func pump(c *websocket.Conn) <-chan string {
	ch := make(chan string, 8)
	go func() {
		defer close(ch)
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &m) == nil {
				ch <- m.Type
			}
		}
	}()
	return ch
}

func expectType(t *testing.T, ch <-chan string, want string, within time.Duration) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("connection closed while waiting for %q", want)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	case <-time.After(within):
		t.Errorf("nothing arrived within %v, want %q", within, want)
	}
}

func expectSilence(t *testing.T, ch <-chan string, within time.Duration) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if ok {
			t.Errorf("received %q, want nothing", got)
		} else {
			t.Error("connection closed, want it left open and silent")
		}
	case <-time.After(within):
	}
}

func send(t *testing.T, c *websocket.Conn, msgType string) {
	t.Helper()
	msg, _ := json.Marshal(map[string]string{"type": msgType})
	if err := c.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

// The server must not volunteer a "ready" when both peers are present. It fired on
// every reconnect, rebuilding a peer connection that was still streaming — precisely
// the teardown this change exists to prevent. Only the client knows whether its
// media is alive, so only the client may ask.
func TestServerDoesNotVolunteerReady(t *testing.T) {
	url := testServer(t)
	host := pump(dial(t, url, "host"))
	dial(t, url, "client")

	expectSilence(t, host, time.Second)
}

// ...but a client-sent "ready" is relayed, because that is now the only thing that
// ever starts a stream.
func TestClientReadyIsRelayedToHost(t *testing.T) {
	url := testServer(t)
	host := pump(dial(t, url, "host"))
	client := dial(t, url, "client")

	send(t, client, "ready")
	expectType(t, host, "ready", 3*time.Second)
}

// A browser reconnecting after its socket was reaped must take the role back. The
// old behaviour refused it ("role already connected") until the corpse timed out,
// turning a recoverable blip into a session the user could not fix by refreshing.
func TestReconnectEvictsStalePeer(t *testing.T) {
	url := testServer(t)
	host := pump(dial(t, url, "host"))
	stale := pump(dial(t, url, "client"))

	fresh, _, err := websocket.Dial(context.Background(), url+"?role=client", nil)
	if err != nil {
		t.Fatalf("reconnecting client was refused: %v", err)
	}
	defer fresh.CloseNow()

	// The displaced socket must actually be closed, not left dangling.
	select {
	case _, ok := <-stale:
		if ok {
			t.Error("displaced connection delivered a message; two clients share a role")
		}
	case <-time.After(3 * time.Second):
		t.Error("displaced connection is still open; two clients share a role")
	}

	// An eviction is not a departure: telling the host the browser left would make
	// it tear down the very session the newcomer is about to use.
	expectSilence(t, host, time.Second)

	// And the fresh socket is the live one.
	send(t, fresh, "ready")
	expectType(t, host, "ready", 3*time.Second)
}

// A genuine departure still reaches the other side. The host no longer tears down
// media on this news, but it must still hear it.
func TestRealDepartureStillNotifiesPeer(t *testing.T) {
	url := testServer(t)
	host := pump(dial(t, url, "host"))
	client := dial(t, url, "client")

	_ = client.CloseNow()
	expectType(t, host, "peer-disconnected", 3*time.Second)
}

// The keepalive is the whole point: this socket carries nothing between the end of
// ICE negotiation and the end of the call, and an idle connection through a school
// firewall, the Cloudflare edge and a cloudflared tunnel gets reaped in minutes.
// A connection that sits silent across a full ping cycle must still be usable.
func TestIdleConnectionSurvivesAPingCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out pingInterval by design")
	}
	url := testServer(t)
	client := dial(t, url, "client")
	msgs := pump(client) // parked reader answers pings, exactly as a browser does

	expectSilence(t, msgs, pingInterval+5*time.Second)

	send(t, client, "ready") // fails if the connection died while idling
}
