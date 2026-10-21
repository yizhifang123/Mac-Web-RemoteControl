// Package ice supplies the ICE server list — STUN plus, when a Cloudflare TURN key
// is configured, short-lived relay credentials minted from Cloudflare Realtime.
// TURN is what gets media through networks that block direct UDP
// (school wifi): Cloudflare's turns:…:443?transport=tcp endpoint looks like ordinary
// HTTPS to a firewall.
//
// The credentials are minted server-side and handed to the browser only through the
// authenticated /ice-servers endpoint — a relay is billable bandwidth, so the creds
// are short-lived and never public. Minting failure is never fatal: the caller gets
// the STUN-only fallback and the session proceeds exactly as before TURN existed.
package ice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	defaultBaseURL = "https://rtc.live.cloudflare.com"

	// credTTL is the lifetime asked of Cloudflare. A day comfortably outlives any
	// session while still expiring leaked credentials on their own.
	credTTL = 24 * time.Hour

	// refreshMargin: re-mint when less than this remains, so credentials handed to
	// a browser are never about to expire mid-session.
	refreshMargin = time.Hour

	// failCooldown: after a failed mint, serve what we have immediately instead of
	// retrying. Servers sits inline in the host's signaling read loop and in every
	// /ice-servers request, so a Cloudflare outage must not turn each browser
	// reconnect into a fresh 10-second stall.
	failCooldown = 30 * time.Second
)

// Fallback is the STUN-only server list used whenever TURN is unconfigured or
// minting fails. Returned fresh each call so callers cannot mutate shared state.
func Fallback() []webrtc.ICEServer {
	return []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
}

// TurnConfig identifies a Cloudflare TURN key (dashboard: Realtime → TURN keys).
type TurnConfig struct {
	KeyID    string
	APIToken string
}

// Enabled reports whether a TURN key is configured at all.
func (c TurnConfig) Enabled() bool { return c.KeyID != "" && c.APIToken != "" }

// Provider mints and caches ICE server lists. The zero value is valid and serves
// the STUN-only fallback; fields are set once at startup, before any Servers call.
type Provider struct {
	TurnConfig

	// BaseURL overrides the Cloudflare endpoint (tests). Empty means production.
	BaseURL string

	// Client overrides the HTTP client (tests). Nil means a 10s-timeout default:
	// Servers sits on the session-setup path, so a hung mint must not hang setup.
	Client *http.Client

	mu       sync.Mutex
	cached   []webrtc.ICEServer
	expires  time.Time
	cooldown time.Time     // after a failed mint: no retries until this passes
	minting  chan struct{} // non-nil while a mint is in flight; closed when it lands
}

// Servers returns the ICE servers a new peer connection should use. It never fails:
// no TURN key or a mint error yields still-valid cached credentials or the STUN-only
// fallback — a session without a relay still works everywhere a session worked
// before TURN existed, so TURN being down must degrade, not refuse.
//
// The mint runs OUTSIDE the lock and is single-flighted: concurrent callers wait for
// the one in-flight request rather than racing Cloudflare, and a caller that only
// needs the cache is never stuck behind a 10-second HTTP timeout.
func (p *Provider) Servers(ctx context.Context) []webrtc.ICEServer {
	if p == nil || !p.Enabled() {
		return Fallback()
	}

	for {
		p.mu.Lock()
		now := time.Now()
		switch {
		case p.cached != nil && p.expires.Sub(now) > refreshMargin:
			// The common case: a cache lookup.
			s := p.cached
			p.mu.Unlock()
			return s

		case now.Before(p.cooldown):
			// A mint just failed; don't stall this session retrying.
			s, exp := p.cached, p.expires
			p.mu.Unlock()
			if s != nil && now.Before(exp) {
				return s // near expiry but still valid beats STUN-only
			}
			return Fallback()

		case p.minting != nil:
			// Someone else is minting: wait for their result, then re-evaluate.
			done := p.minting
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return Fallback()
			}
		}

		done := make(chan struct{})
		p.minting = done
		p.mu.Unlock()

		servers, expires, err := p.mint(ctx)

		p.mu.Lock()
		p.minting = nil
		if err != nil {
			p.cooldown = time.Now().Add(failCooldown)
			s, exp := p.cached, p.expires
			p.mu.Unlock()
			close(done)
			log.Printf("TURN credentials unavailable (retrying after %s): %v", failCooldown, err)
			if s != nil && time.Now().Before(exp) {
				return s
			}
			return Fallback()
		}
		p.cached, p.expires, p.cooldown = servers, expires, time.Time{}
		p.mu.Unlock()
		close(done)
		return servers
	}
}

// mint asks Cloudflare for a fresh ICE server list carrying TURN credentials, and
// returns when the credentials stop being trustworthy.
func (p *Provider) mint(ctx context.Context) ([]webrtc.ICEServer, time.Time, error) {
	base := p.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	url := base + "/v1/turn/keys/" + p.KeyID + "/credentials/generate-ice-servers"

	body, err := json.Marshal(map[string]int64{"ttl": int64(credTTL / time.Second)})
	if err != nil {
		return nil, time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIToken)
	req.Header.Set("Content-Type", "application/json")

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Expiry is measured from BEFORE the request: Cloudflare's clock starts when it
	// issues the credentials, so booking now+ttl after the round-trip would credit
	// them with time already spent in flight.
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint TURN credentials: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, time.Time{}, fmt.Errorf("cloudflare TURN mint returned %s: %s",
			resp.Status, bytes.TrimSpace(snippet))
	}

	// Cloudflare's response entries use the same urls/username/credential JSON keys
	// as webrtc.ICEServer's tags, so they unmarshal directly (verified against
	// pion/webrtc v4.2.17 iceserver.go). The lifetime fields are tolerated if
	// absent; when one is present and SHORTER than what we asked for, the granted
	// value wins — trusting the request would cache dead credentials.
	var out struct {
		ICEServers []webrtc.ICEServer `json:"iceServers"`
		TTL        int64              `json:"ttl"`
		Lifetime   int64              `json:"lifetime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode cloudflare TURN response: %w", err)
	}
	if len(out.ICEServers) == 0 {
		return nil, time.Time{}, fmt.Errorf("cloudflare TURN response contained no iceServers")
	}
	granted := credTTL
	for _, secs := range []int64{out.TTL, out.Lifetime} {
		if d := time.Duration(secs) * time.Second; secs > 0 && d < granted {
			granted = d
		}
	}
	return out.ICEServers, start.Add(granted), nil
}
