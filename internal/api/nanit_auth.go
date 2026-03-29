package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/nanit"
)

type nanitAuthManager struct {
	tokenMgr *nanit.TokenManager
	manager  *baby.Manager
	onAuth   func() error

	mu           sync.Mutex
	pendingMFATo string
}

func newNanitAuthManager(tokenMgr *nanit.TokenManager, manager *baby.Manager, onAuth func() error) *nanitAuthManager {
	return &nanitAuthManager{
		tokenMgr: tokenMgr,
		manager:  manager,
		onAuth:   onAuth,
	}
}

func (n *nanitAuthManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session := n.tokenMgr.GetSession()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected": n.manager.IsStarted(),
		"email":     session.Email,
	})
}

func (n *nanitAuthManager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	if body.Email == "" || body.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}

	n.tokenMgr.SetCredentials(body.Email, body.Password)
	n.mu.Lock()
	n.pendingMFATo = ""
	n.mu.Unlock()
	mfaToken, err := n.tokenMgr.Login()
	if err != nil {
		http.Error(w, "invalid credentials or login failed", http.StatusUnauthorized)
		return
	}

	if mfaToken != "" {
		n.mu.Lock()
		n.pendingMFATo = mfaToken
		n.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "mfa_required"})
		return
	}

	if n.onAuth != nil {
		if err := n.onAuth(); err != nil {
			http.Error(w, "authenticated but failed to start manager", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (n *nanitAuthManager) handleMFA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	body.Code = strings.TrimSpace(body.Code)
	if body.Code == "" {
		http.Error(w, "mfa code is required", http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	mfaToken := n.pendingMFATo
	n.mu.Unlock()
	if mfaToken == "" {
		http.Error(w, "no MFA challenge pending", http.StatusBadRequest)
		return
	}

	if err := n.tokenMgr.LoginWithMFA(mfaToken, body.Code); err != nil {
		http.Error(w, "MFA verification failed", http.StatusUnauthorized)
		return
	}

	n.mu.Lock()
	n.pendingMFATo = ""
	n.mu.Unlock()

	if n.onAuth != nil {
		if err := n.onAuth(); err != nil {
			http.Error(w, "authenticated but failed to start manager", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
