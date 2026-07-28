// Package auth is the gate in front of the signaling server. This tool injects
// keystrokes and clicks, so it is remote-code-execution by design (docs/SECURITY.md security
// model): nothing may reach signaling — not the client page, not the WebSocket
// upgrade — without passing through here.
//
// Two ways in, one check:
//
//   - a browser posts the password once and gets an HMAC-signed session cookie,
//     which it then sends automatically on the same-origin WebSocket upgrade;
//   - the local host process presents "Authorization: Bearer <token>".
//
// There is deliberately no loopback exemption. One code path, nothing to get wrong
// about whether a connection "looks local".
//
// Secrets live in a 0600 JSON file (default ~/.config/play/config.json). The password
// is stored ONLY as a bcrypt hash — it is printed once, at creation, and cannot be
// recovered afterwards (regenerate with -new-password).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "play_session"

	// SessionTTL is how long a browser login lasts. Short by design: a stolen
	// laptop or a shared browser should not hold control of the Mac indefinitely.
	SessionTTL = 24 * time.Hour

	// Brute-force throttle. A generated password has 96 bits of entropy and is
	// unguessable, but a user-chosen one may not be.
	maxFails    = 10
	lockoutFor  = time.Minute
	failMinWait = 250 * time.Millisecond

	// Per-IP throttling alone is evaded by an attacker with many source addresses,
	// so failures are ALSO capped globally. Set far above any human's fumbling but
	// far below what makes a dictionary attack practical: at this rate a memorable
	// password still takes years, while a legitimate user never notices it.
	globalFailsPerMin = 30
)

// stored is the on-disk secret file (mode 0600).
type stored struct {
	PasswordHash string `json:"password_hash"` // bcrypt; plaintext is never written
	Token        string `json:"token"`         // bearer token for the local host process
	CookieSecret string `json:"cookie_secret"` // HMAC key for session cookies
}

// Auth validates browser sessions and the host process's bearer token.
type Auth struct {
	hash   []byte
	token  string
	secret []byte

	mu           sync.Mutex
	fails        map[string]*attempts
	globalFails  int
	globalWindow time.Time
}

type attempts struct {
	n     int
	until time.Time
}

// DefaultConfigPath is where secrets live unless overridden.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".config", "play", "config.json")
}

// Load reads the secret file, creating it if absent (or if regenerate is set).
// The returned password is non-empty ONLY when a new one was generated — print it
// then, because it is not recoverable afterwards.
func Load(path string, regenerate bool) (a *Auth, newPassword string, err error) {
	var s stored
	raw, readErr := os.ReadFile(path)
	switch {
	case readErr == nil && !regenerate:
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", path, err)
		}
		if s.PasswordHash == "" || s.Token == "" || s.CookieSecret == "" {
			return nil, "", fmt.Errorf("%s is missing fields; delete it to regenerate", path)
		}
	case readErr == nil || errors.Is(readErr, os.ErrNotExist):
		newPassword = randomSecret(12) // 96 bits
		hash, herr := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if herr != nil {
			return nil, "", herr
		}
		s = stored{
			PasswordHash: string(hash),
			Token:        randomSecret(32),
			CookieSecret: randomSecret(32),
		}
		if err := write(path, s); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", readErr
	}

	secret, err := base64.RawURLEncoding.DecodeString(s.CookieSecret)
	if err != nil {
		return nil, "", fmt.Errorf("bad cookie_secret in %s: %w", path, err)
	}
	return &Auth{
		hash:   []byte(s.PasswordHash),
		token:  s.Token,
		secret: secret,
		fails:  map[string]*attempts{},
	}, newPassword, nil
}

// SetPassword replaces the stored password with one the user chose, and rotates the
// cookie secret so every existing session is invalidated — changing a password must
// log out anyone already holding a session, including a stolen one. The host's
// bearer token is left alone so a running session's own plumbing keeps working.
//
// Returns a human-readable warning when the password is weak enough to be worth
// saying something about; it does not refuse (it is the owner's machine and call).
func SetPassword(path, password string) (warning string, err error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("password is empty")
	}

	var s stored
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return "", rerr
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	s.PasswordHash = string(hash)
	s.CookieSecret = randomSecret(32) // invalidate every existing session
	if s.Token == "" {
		s.Token = randomSecret(32)
	}
	if err := write(path, s); err != nil {
		return "", err
	}

	if len(password) < 12 {
		warning = fmt.Sprintf("%d characters is short for an internet-facing gate on a tool "+
			"that can type into this Mac; 16+ is a better floor.", len(password))
	}
	return warning, nil
}

func write(path string, s stored) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file is the whole perimeter.
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Token is the bearer token the local host process uses to reach signaling.
func (a *Auth) Token() string { return a.token }

// Protect wraps next so nothing is reachable without a valid session or token.
// loginPage is served (unauthenticated) at /login.
func (a *Auth) Protect(next http.Handler, loginPage []byte) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if a.Authorized(r) {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(loginPage)
		case http.MethodPost:
			a.handleLogin(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		// A WebSocket dialer cannot act on a redirect; fail it outright.
		if isWebSocketUpgrade(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}))

	return mux
}

// Authorized reports whether r carries a valid session cookie or bearer token.
func (a *Auth) Authorized(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) == 1 {
			return true
		}
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return a.validCookie(c.Value)
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait, locked := a.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(w, "too many attempts; wait a minute", http.StatusTooManyRequests)
		return
	}
	if a.globallyThrottled() {
		http.Error(w, "too many attempts server-wide; try again shortly", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if bcrypt.CompareHashAndPassword(a.hash, []byte(r.PostForm.Get("password"))) != nil {
		a.recordFailure(ip)
		time.Sleep(failMinWait) // blunt the request rate even before lockout
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	a.clearFailures(ip)
	exp := time.Now().Add(SessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.signSession(exp),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,                 // never readable by page JS
		Secure:   requestIsHTTPS(r),    // set only when the browser used HTTPS
		SameSite: http.SameSiteLaxMode, // blocks cross-site POSTs at the gate
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signSession returns "<expiryUnix>.<hmac>" — no server-side session store needed.
func (a *Auth) signSession(exp time.Time) string {
	unix := strconv.FormatInt(exp.Unix(), 10)
	return unix + "." + hex.EncodeToString(a.mac(unix))
}

func (a *Auth) validCookie(v string) bool {
	unix, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(want, a.mac(unix)) {
		return false
	}
	exp, err := strconv.ParseInt(unix, 10, 64)
	return err == nil && time.Now().Before(time.Unix(exp, 0))
}

func (a *Auth) mac(msg string) []byte {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func (a *Auth) locked(ip string) (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.fails[ip]
	if !ok || at.until.IsZero() {
		return 0, false
	}
	if wait := time.Until(at.until); wait > 0 {
		return wait, true
	}
	return 0, false
}

// globallyThrottled reports whether failures across ALL clients have exceeded the
// per-minute cap. This is what makes a distributed guessing attack impractical when
// the password is human-memorable rather than random.
func (a *Auth) globallyThrottled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.globalWindow) >= time.Minute {
		a.globalWindow = time.Now()
		a.globalFails = 0
	}
	return a.globalFails >= globalFailsPerMin
}

func (a *Auth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.globalFails++
	at, ok := a.fails[ip]
	if !ok {
		at = &attempts{}
		a.fails[ip] = at
	}
	at.n++
	if at.n >= maxFails {
		at.until = time.Now().Add(lockoutFor)
		at.n = 0
	}
}

func (a *Auth) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

func clientIP(r *http.Request) string {
	// Behind the tunnel every request arrives from 127.0.0.1, so per-IP throttling
	// would collapse into one global bucket; CF-Connecting-IP restores per-client
	// granularity. It is attacker-controlled on a direct connection, which only ever
	// splits an attacker's own bucket — it can never bypass someone else's lockout.
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
