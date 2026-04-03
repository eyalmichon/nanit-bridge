package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	sessionCookieName = "nanit_session"
	sessionMaxAge     = 7 * 24 * time.Hour

	maxLoginFailures = 5
	loginLockoutTime = 30 * time.Second
	loginEntryMaxAge = 5 * time.Minute
)

type loginAttempt struct {
	failures int
	lastFail time.Time
}

type authManager struct {
	authFile      string
	mu            sync.Mutex
	loginAttempts map[string]*loginAttempt
}

func newAuthManager(authFile string) *authManager {
	return &authManager{
		authFile:      authFile,
		loginAttempts: make(map[string]*loginAttempt),
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoginLocked checks whether ip has exceeded the failure threshold
// and the lockout window has not yet expired.
func (a *authManager) isLoginLocked(ip string) (bool, time.Duration) {
	entry := a.loginAttempts[ip]
	if entry == nil || entry.failures < maxLoginFailures {
		return false, 0
	}
	remaining := loginLockoutTime - time.Since(entry.lastFail)
	if remaining <= 0 {
		delete(a.loginAttempts, ip)
		return false, 0
	}
	return true, remaining
}

func (a *authManager) recordLoginFailure(ip string) {
	entry := a.loginAttempts[ip]
	if entry == nil {
		entry = &loginAttempt{}
		a.loginAttempts[ip] = entry
	}
	entry.failures++
	entry.lastFail = time.Now()
}

func (a *authManager) clearLoginFailures(ip string) {
	delete(a.loginAttempts, ip)
}

func (a *authManager) pruneLoginAttempts() {
	cutoff := time.Now().Add(-loginEntryMaxAge)
	for ip, entry := range a.loginAttempts {
		if entry.lastFail.Before(cutoff) {
			delete(a.loginAttempts, ip)
		}
	}
}

func (a *authManager) readHashFromDisk() ([]byte, error) {
	return os.ReadFile(a.authFile)
}

func (a *authManager) hasPasswordHash() bool {
	_, err := os.Stat(a.authFile)
	return err == nil
}

func (a *authManager) writeHash(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bcrypt error: %w", err)
	}
	if dir := filepath.Dir(a.authFile); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir auth dir: %w", err)
		}
	}
	if err := os.WriteFile(a.authFile, hash, 0o600); err != nil {
		return fmt.Errorf("write auth file: %w", err)
	}
	return nil
}

func (a *authManager) checkPassword(password string) (bool, error) {
	hash, err := a.readHashFromDisk()
	if err != nil {
		return false, fmt.Errorf("read auth file: %w", err)
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil, nil
}

// signedToken creates a cookie value: "expiry_unix.hmac_hex".
// The HMAC key is derived from the bcrypt hash on disk, so changing
// the password automatically invalidates all existing cookies.
func (a *authManager) signedToken() (string, error) {
	key, err := a.readHashFromDisk()
	if err != nil {
		return "", err
	}
	expiry := time.Now().Add(sessionMaxAge).Unix()
	payload := strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", payload, sig), nil
}

func (a *authManager) validateToken(token string) (bool, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false, nil
	}
	payload, sig := parts[0], parts[1]

	expiry, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false, nil
	}

	key, err := a.readHashFromDisk()
	if err != nil {
		return false, fmt.Errorf("read auth file: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected)), nil
}

func (a *authManager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := remoteIP(r)

	a.mu.Lock()
	a.pruneLoginAttempts()
	locked, remaining := a.isLoginLocked(ip)
	a.mu.Unlock()

	if locked {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(int(remaining.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "too many failed attempts, try again later"})
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	ok, err := a.checkPassword(body.Password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		a.mu.Lock()
		a.recordLoginFailure(ip)
		a.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid password"})
		return
	}

	a.mu.Lock()
	a.clearLoginFailures(ip)
	a.mu.Unlock()

	token, err := a.signedToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *authManager) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	body.Password = strings.TrimSpace(body.Password)
	if body.Password == "" || body.Confirm == "" {
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}
	if body.Password != body.Confirm {
		http.Error(w, "passwords do not match", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hasPasswordHash() {
		http.Error(w, "dashboard password already configured", http.StatusConflict)
		return
	}
	if err := a.writeHash(body.Password); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *authManager) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if body.CurrentPassword == "" || body.NewPassword == "" || body.ConfirmPassword == "" {
		http.Error(w, "all password fields are required", http.StatusBadRequest)
		return
	}
	if body.NewPassword != body.ConfirmPassword {
		http.Error(w, "new passwords do not match", http.StatusBadRequest)
		return
	}
	ok, err := a.checkPassword(body.CurrentPassword)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "invalid current password", http.StatusUnauthorized)
		return
	}
	if err := a.writeHash(body.NewPassword); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Invalidate existing cookie after password change.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *authManager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// authMiddleware wraps a handler and enforces session auth on all routes except
// the login page, login API, and shared static assets.
func (a *authManager) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "/health" || path == "/api/version" {
			next.ServeHTTP(w, r)
			return
		}

		if !a.hasPasswordHash() {
			if path == "/setup" || path == "/setup/" ||
				strings.HasPrefix(path, "/setup/") ||
				path == "/api/auth/setup" ||
				strings.HasPrefix(path, "/shared/") ||
				path == "/favicon.ico" {
				next.ServeHTTP(w, r)
				return
			}
			if isAPIRequest(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]string{"error": "setup_required"})
			} else {
				http.Redirect(w, r, "/setup", http.StatusFound)
			}
			return
		}

		if path == "/login" || path == "/login/" ||
			strings.HasPrefix(path, "/login/") ||
			path == "/api/auth/login" ||
			strings.HasPrefix(path, "/shared/") ||
			path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		c, cookieErr := r.Cookie(sessionCookieName)
		if cookieErr != nil {
			if isAPIRequest(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			} else {
				http.Redirect(w, r, "/login", http.StatusFound)
			}
			return
		}
		valid, authErr := a.validateToken(c.Value)
		if authErr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !valid {
			if isAPIRequest(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			} else {
				http.Redirect(w, r, "/login", http.StatusFound)
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json")
}
