package nanit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func withTestAPIBase(t *testing.T, base string) {
	t.Helper()
	old := apiBase
	apiBase = base
	t.Cleanup(func() { apiBase = old })
}

func TestTokenManagerLoginSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "acc-1",
			"refresh_token": "ref-1",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()
	withTestAPIBase(t, ts.URL)

	tm := NewTokenManager("u@example.com", "pass", t.TempDir()+"/session.json")
	tm.client = ts.Client()

	mfaToken, err := tm.Login()
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if mfaToken != "" {
		t.Fatalf("mfaToken = %q, want empty", mfaToken)
	}
	s := tm.GetSession()
	if s.AccessToken != "acc-1" || s.RefreshToken != "ref-1" {
		t.Fatalf("unexpected session: %+v", s)
	}
}

func TestTokenManagerLoginMFARequired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(482)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"mfa_token": "mfa-abc",
		})
	}))
	defer ts.Close()
	withTestAPIBase(t, ts.URL)

	tm := NewTokenManager("u@example.com", "pass", t.TempDir()+"/session.json")
	tm.client = ts.Client()

	mfaToken, err := tm.Login()
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if mfaToken != "mfa-abc" {
		t.Fatalf("mfaToken = %q, want mfa-abc", mfaToken)
	}
}

func TestTokenManagerGetAccessTokenRefreshes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokens/refresh" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "acc-new",
			"refresh_token": "ref-new",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()
	withTestAPIBase(t, ts.URL)

	tm := NewTokenManager("u@example.com", "pass", t.TempDir()+"/session.json")
	tm.client = ts.Client()
	tm.session.AccessToken = "acc-old"
	tm.session.RefreshToken = "ref-old"
	tm.session.ExpiresAt = time.Now().Add(-time.Minute)

	token, err := tm.GetAccessToken()
	if err != nil {
		t.Fatalf("GetAccessToken() error: %v", err)
	}
	if token != "acc-new" {
		t.Fatalf("token = %q, want acc-new", token)
	}
}

func TestTokenManagerFetchBabiesParsesAndCaches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/babies" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"babies": []map[string]interface{}{
				{
					"uid":  "baby-1",
					"name": "Ava",
					"camera": map[string]interface{}{
						"uid": "cam-1",
					},
				},
			},
		})
	}))
	defer ts.Close()
	withTestAPIBase(t, ts.URL)

	tm := NewTokenManager("u@example.com", "pass", t.TempDir()+"/session.json")
	tm.client = ts.Client()
	tm.session.AccessToken = "acc-1"
	tm.session.ExpiresAt = time.Now().Add(time.Hour)

	babies, err := tm.FetchBabies()
	if err != nil {
		t.Fatalf("FetchBabies() error: %v", err)
	}
	if len(babies) != 1 {
		t.Fatalf("len(babies) = %d, want 1", len(babies))
	}
	if babies[0].UID != "baby-1" || babies[0].CameraUID != "cam-1" {
		t.Fatalf("unexpected baby: %+v", babies[0])
	}
	if len(tm.GetSession().Babies) != 1 {
		t.Fatalf("expected cached babies in session")
	}
}
