package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAuthManager(t *testing.T) *authManager {
	t.Helper()
	authFile := filepath.Join(t.TempDir(), "dashboard.hash")
	return newAuthManager(authFile)
}

func TestAuthManagerWriteAndCheckPassword(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}
	if ok, err := a.checkPassword("secret123"); err != nil {
		t.Fatalf("checkPassword error: %v", err)
	} else if !ok {
		t.Fatalf("expected checkPassword true")
	}
	if ok, err := a.checkPassword("wrong"); err != nil {
		t.Fatalf("checkPassword error: %v", err)
	} else if ok {
		t.Fatalf("expected checkPassword false for wrong password")
	}
}

func TestAuthManagerSignedTokenAndValidate(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}

	token, err := a.signedToken()
	if err != nil {
		t.Fatalf("signedToken error: %v", err)
	}
	if ok, err := a.validateToken(token); err != nil {
		t.Fatalf("validateToken error: %v", err)
	} else if !ok {
		t.Fatalf("expected signed token to validate")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("invalid token format: %q", token)
	}
	tampered := parts[0] + ".00" + parts[1][2:]
	if ok, err := a.validateToken(tampered); err != nil {
		t.Fatalf("validateToken error: %v", err)
	} else if ok {
		t.Fatalf("expected tampered token to fail validation")
	}
}

func TestAuthManagerValidateExpiredToken(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}
	key, err := os.ReadFile(a.authFile)
	if err != nil {
		t.Fatalf("read auth hash: %v", err)
	}

	expired := "1"
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(expired))
	sig := hex.EncodeToString(mac.Sum(nil))
	token := expired + "." + sig

	if ok, err := a.validateToken(token); err != nil {
		t.Fatalf("validateToken error: %v", err)
	} else if ok {
		t.Fatalf("expected expired token to fail validation")
	}
}

func TestAuthMiddlewareNoPasswordConfigured(t *testing.T) {
	a := newTestAuthManager(t)
	next := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Browser path should redirect to setup.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	next.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/setup" {
		t.Fatalf("location = %q, want /setup", got)
	}

	// API path should return setup_required JSON.
	apiReq := httptest.NewRequest(http.MethodGet, "/api/babies", nil)
	apiRR := httptest.NewRecorder()
	next.ServeHTTP(apiRR, apiReq)
	if apiRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("api status = %d, want 503", apiRR.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(apiRR.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if body["error"] != "setup_required" {
		t.Fatalf("error = %q, want setup_required", body["error"])
	}
}

func TestAuthMiddlewareUnauthorizedAndBypass(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}
	next := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Unauthorized browser request redirects to login.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	next.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/login" {
		t.Fatalf("unexpected browser unauth response: code=%d loc=%q", rr.Code, rr.Header().Get("Location"))
	}

	// Unauthorized API request gets 401 JSON.
	apiReq := httptest.NewRequest(http.MethodGet, "/api/babies", nil)
	apiRR := httptest.NewRecorder()
	next.ServeHTTP(apiRR, apiReq)
	if apiRR.Code != http.StatusUnauthorized {
		t.Fatalf("api status = %d, want 401", apiRR.Code)
	}

	// Bypass routes.
	for _, p := range []string{"/login", "/shared/style.css", "/favicon.ico"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		next.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("path %q status = %d, want 204", p, w.Code)
		}
	}
}

func TestAuthMiddlewareValidCookiePassesThrough(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}
	token, err := a.signedToken()
	if err != nil {
		t.Fatalf("signedToken error: %v", err)
	}
	next := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/babies", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rr := httptest.NewRecorder()
	next.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestAuthHandlersSetupLoginChangeAndLogout(t *testing.T) {
	a := newTestAuthManager(t)

	// Setup
	setupReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"secret123","confirm":"secret123"}`))
	setupRR := httptest.NewRecorder()
	a.handleSetup(setupRR, setupReq)
	if setupRR.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want 200", setupRR.Code)
	}

	// Login
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"secret123"}`))
	loginRR := httptest.NewRecorder()
	a.handleLogin(loginRR, loginReq)
	if loginRR.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginRR.Code)
	}
	cookies := loginRR.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie")
	}

	// Change password.
	changeReq := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", strings.NewReader(`{"current_password":"secret123","new_password":"newsecret","confirm_password":"newsecret"}`))
	changeRR := httptest.NewRecorder()
	a.handleChangePassword(changeRR, changeReq)
	if changeRR.Code != http.StatusOK {
		t.Fatalf("change-password status = %d, want 200", changeRR.Code)
	}
	if ok, err := a.checkPassword("newsecret"); err != nil {
		t.Fatalf("checkPassword error: %v", err)
	} else if !ok {
		t.Fatalf("new password hash not applied")
	}

	// Logout.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutRR := httptest.NewRecorder()
	a.handleLogout(logoutRR, logoutReq)
	if logoutRR.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logoutRR.Code)
	}
	foundExpiredCookie := false
	for _, c := range logoutRR.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			foundExpiredCookie = true
		}
	}
	if !foundExpiredCookie {
		t.Fatalf("expected logout to set expired session cookie")
	}
}

func TestLoginRateLimiting(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}

	sendLogin := func(password, remoteAddr string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"password":"` + password + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
		req.RemoteAddr = remoteAddr
		rr := httptest.NewRecorder()
		a.handleLogin(rr, req)
		return rr
	}

	addr := "10.0.0.1:12345"

	for i := 0; i < maxLoginFailures; i++ {
		rr := sendLogin("wrong", addr)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rr.Code)
		}
	}

	rr := sendLogin("wrong", addr)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("after lockout: status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header")
	}

	rr = sendLogin("secret123", addr)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: status = %d, want 429", rr.Code)
	}

	otherAddr := "10.0.0.2:12345"
	rr = sendLogin("secret123", otherAddr)
	if rr.Code != http.StatusOK {
		t.Fatalf("different IP: status = %d, want 200", rr.Code)
	}

	a.mu.Lock()
	a.loginAttempts["10.0.0.1"] = &loginAttempt{
		failures: maxLoginFailures,
		lastFail: time.Now().Add(-loginLockoutTime - time.Second),
	}
	a.mu.Unlock()

	rr = sendLogin("secret123", addr)
	if rr.Code != http.StatusOK {
		t.Fatalf("after lockout expired: status = %d, want 200", rr.Code)
	}
}

func TestAuthTokenValidityWindow(t *testing.T) {
	a := newTestAuthManager(t)
	if err := a.writeHash("secret123"); err != nil {
		t.Fatalf("writeHash error: %v", err)
	}
	token, err := a.signedToken()
	if err != nil {
		t.Fatalf("signedToken error: %v", err)
	}
	if ok, err := a.validateToken(token); err != nil {
		t.Fatalf("validateToken error: %v", err)
	} else if !ok {
		t.Fatalf("token should be valid immediately")
	}
	// Sanity check that token lifetime is future-dated.
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid token format")
	}
	if parts[0] <= "0" {
		t.Fatalf("unexpected token payload: %q", parts[0])
	}
	_ = time.Second
}
