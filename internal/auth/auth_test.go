package auth

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestAuth creates a gate in a temp dir and returns it with its fresh password.
func newTestAuth(t *testing.T) (*Auth, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	a, password, err := Load(path, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if password == "" {
		t.Fatal("expected a generated password on first run")
	}
	return a, password, path
}

// protected is the gate wrapped around a handler that marks anything reaching it.
func protected(a *Auth) http.Handler {
	return a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("REACHED"))
	}), []byte("LOGIN PAGE"))
}

func TestSecretsFileIsPrivateAndHashed(t *testing.T) {
	a, password, path := newTestAuth(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Error("plaintext password was written to disk; only the bcrypt hash should be stored")
	}
	if a.Token() == "" {
		t.Error("no host token generated")
	}
}

func TestLoadIsStableAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first, password, err := Load(path, false)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, again, err := Load(path, false)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again != "" {
		t.Error("second Load regenerated the password; a restart must not invalidate it")
	}
	if first.Token() != second.Token() {
		t.Error("host token changed across restarts")
	}
	// A cookie issued before the restart must still validate after it.
	if !second.validCookie(first.signSession(time.Now().Add(time.Hour))) {
		t.Error("session from before restart no longer valid; cookie secret was not persisted")
	}
	if _, regenerated, _ := Load(path, true); regenerated == password || regenerated == "" {
		t.Error("-new-password did not produce a different password")
	}
}

func TestUnauthenticatedIsBlocked(t *testing.T) {
	a, _, _ := newTestAuth(t)
	h := protected(a)

	cases := []struct {
		name, path string
		websocket  bool
		wantCode   int
	}{
		{name: "client page redirects to login", path: "/", wantCode: http.StatusSeeOther},
		{name: "index.html redirects to login", path: "/index.html", wantCode: http.StatusSeeOther},
		{name: "websocket upgrade is refused", path: "/ws", websocket: true, wantCode: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.websocket {
				r.Header.Set("Upgrade", "websocket")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if strings.Contains(w.Body.String(), "REACHED") {
				t.Error("request reached the protected handler without authentication")
			}
		})
	}
}

func TestLoginIssuesWorkingSession(t *testing.T) {
	a, password, _ := newTestAuth(t)
	h := protected(a)

	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie issued")
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly; page JS could read it")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie is not SameSite=Lax")
	}

	// The cookie must then open both the page and the WebSocket upgrade.
	for _, path := range []string{"/", "/ws"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(c)
		if path == "/ws" {
			r.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), "REACHED") {
			t.Errorf("%s: valid session did not reach the handler (status %d)", path, w.Code)
		}
	}
}

func TestWrongPasswordIssuesNothing(t *testing.T) {
	a, password, _ := newTestAuth(t)
	h := protected(a)

	form := url.Values{"password": {password + "x"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if len(w.Result().Cookies()) != 0 {
		t.Fatal("a session cookie was issued for a wrong password")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error") {
		t.Errorf("expected redirect back to the login form, got %q", loc)
	}
}

func TestForgedAndExpiredCookiesRejected(t *testing.T) {
	a, _, _ := newTestAuth(t)
	other, _, _ := newTestAuth(t) // a different install, different cookie secret

	valid := a.signSession(time.Now().Add(time.Hour))
	unix, sig, _ := strings.Cut(valid, ".")

	cases := []struct {
		name, cookie string
	}{
		{"empty", ""},
		{"garbage", "nonsense"},
		{"no signature", unix},
		{"tampered signature", unix + "." + strings.Repeat("0", len(sig))},
		{"extended expiry, old signature", strconv.FormatInt(time.Now().Add(999*time.Hour).Unix(), 10) + "." + sig},
		{"expired but correctly signed", a.signSession(time.Now().Add(-time.Minute))},
		{"signed by another install", other.signSession(time.Now().Add(time.Hour))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if a.validCookie(tc.cookie) {
				t.Errorf("accepted invalid session cookie %q", tc.cookie)
			}
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Header.Set("Upgrade", "websocket")
			r.AddCookie(&http.Cookie{Name: cookieName, Value: tc.cookie})
			w := httptest.NewRecorder()
			protected(a).ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("websocket status = %d, want 401", w.Code)
			}
		})
	}
	if !a.validCookie(valid) {
		t.Error("a freshly signed session was rejected")
	}
}

func TestBearerToken(t *testing.T) {
	a, _, _ := newTestAuth(t)
	h := protected(a)

	for _, tc := range []struct {
		name, header string
		want         bool
	}{
		{"correct token", "Bearer " + a.Token(), true},
		{"wrong token", "Bearer " + a.Token() + "x", false},
		{"empty bearer", "Bearer ", false},
		{"no prefix", a.Token(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Authorization", tc.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			got := strings.Contains(w.Body.String(), "REACHED")
			if got != tc.want {
				t.Errorf("reached handler = %v, want %v (status %d)", got, tc.want, w.Code)
			}
		})
	}
}

func TestLoginPageIsReachableUnauthenticated(t *testing.T) {
	a, _, _ := newTestAuth(t)
	w := httptest.NewRecorder()
	protected(a).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "LOGIN PAGE") {
		t.Fatalf("login page unreachable: status %d body %q", w.Code, w.Body.String())
	}
}

func TestBruteForceLockout(t *testing.T) {
	a, password, _ := newTestAuth(t)
	h := protected(a)

	attempt := func(pw string) int {
		form := url.Values{"password": {pw}}
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := 0; i < maxFails; i++ {
		attempt("wrong")
	}
	if code := attempt(password); code != http.StatusTooManyRequests {
		t.Errorf("after %d failures status = %d, want 429 (even the correct password waits)", maxFails, code)
	}

	// A different client must not be caught in someone else's lockout.
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "198.51.100.7:5678"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if len(w.Result().Cookies()) == 0 {
		t.Error("an unrelated client was locked out by another IP's failures")
	}
}

// The embedded client must ship a login page, or the gate cannot render itself.
func TestEmbeddedLoginPageExists(t *testing.T) {
	if _, err := fs.Stat(os.DirFS("../.."), "web/login.html"); err != nil {
		t.Fatalf("web/login.html missing: %v", err)
	}
}

func TestSetPasswordInvalidatesSessionsAndKeepsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before, generated, err := Load(path, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	oldSession := before.signSession(time.Now().Add(time.Hour))

	if _, err := SetPassword(path, "a-chosen-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	after, _, err := Load(path, false)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if after.validCookie(oldSession) {
		t.Error("a session issued before the password change still works; a password change must sign everyone out")
	}
	if after.Token() != before.Token() {
		t.Error("host bearer token changed; a password change should not break the running host")
	}

	h := protected(after)
	login := func(pw string) int {
		form := url.Values{"password": {pw}}
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return len(w.Result().Cookies())
	}
	if login("a-chosen-password") == 0 {
		t.Error("the new password does not work")
	}
	if login(generated) != 0 {
		t.Error("the OLD password still works after being replaced")
	}
	if _, err := SetPassword(path, "   "); err == nil {
		t.Error("an empty password was accepted")
	}
}

func TestSetPasswordWarnsOnShortPasswords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if warning, _ := SetPassword(path, "short"); warning == "" {
		t.Error("expected a warning for a 5-character password")
	}
	if warning, _ := SetPassword(path, "a-perfectly-long-passphrase"); warning != "" {
		t.Errorf("unexpected warning for a long password: %q", warning)
	}
}

func TestGlobalThrottleCapsDistributedGuessing(t *testing.T) {
	a, password, _ := newTestAuth(t)
	h := protected(a)

	// Every attempt comes from a DIFFERENT address, so per-IP lockout never fires;
	// only the global cap can stop this.
	attempt := func(i int, pw string) int {
		form := url.Values{"password": {pw}}
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("CF-Connecting-IP", "203.0.113."+strconv.Itoa(i%256))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < globalFailsPerMin; i++ {
		attempt(i, "wrong")
	}
	if code := attempt(999, password); code != http.StatusTooManyRequests {
		t.Errorf("status = %d after %d distributed failures, want 429", code, globalFailsPerMin)
	}
}
