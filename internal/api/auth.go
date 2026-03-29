package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "nanit_session"
	sessionMaxAge     = 7 * 24 * time.Hour
)

type authManager struct {
	authFile string
}

func newAuthManager(authFile string) *authManager {
	return &authManager{authFile: authFile}
}

func (a *authManager) readHashFromDisk() ([]byte, error) {
	return os.ReadFile(a.authFile)
}

func (a *authManager) checkPassword(password string) bool {
	hash, err := a.readHashFromDisk()
	if err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
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

func (a *authManager) validateToken(token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, sig := parts[0], parts[1]

	expiry, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}

	key, err := a.readHashFromDisk()
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func (a *authManager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if !a.checkPassword(body.Password) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid password"})
		return
	}

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

		if path == "/login" || path == "/login/" ||
			strings.HasPrefix(path, "/login/") ||
			path == "/api/auth/login" ||
			strings.HasPrefix(path, "/shared/") ||
			path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		c, err := r.Cookie(sessionCookieName)
		if err != nil || !a.validateToken(c.Value) {
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
