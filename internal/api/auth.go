package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "nanit_session"
	sessionMaxAge     = 7 * 24 * time.Hour
)

type session struct {
	expiresAt time.Time
}

type authManager struct {
	authFile string

	mu       sync.Mutex
	sessions map[string]*session
}

func newAuthManager(authFile string) *authManager {
	return &authManager{
		authFile: authFile,
		sessions: make(map[string]*session),
	}
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

func (a *authManager) createSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = &session{expiresAt: time.Now().Add(sessionMaxAge)}
	return token
}

func (a *authManager) validateSession(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(s.expiresAt) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *authManager) deleteSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
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

	token := a.createSession()
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

	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.deleteSession(c.Value)
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
// the login page, login API, and static login assets.
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
		if err != nil || !a.validateSession(c.Value) {
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
