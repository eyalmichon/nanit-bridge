package nanit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"sync"
	"time"
)

const (
	apiVersion = "1"
	// Nanit requires a mobile-style User-Agent for MFA to work.
	userAgent = "Nanit/767 CFNetwork/1568.200.51 Darwin/24.1.0"

	// Refresh 5 minutes before assumed expiry.
	tokenRefreshBuffer = 5 * time.Minute
	// Default assumed TTL if the API doesn't return one.
	defaultTokenTTL = 55 * time.Minute
)

var apiBase = "https://api.nanit.com"

// SetAPIBaseForTest overrides the Nanit API base URL and returns a restore func.
// Intended for integration/E2E tests that run against a mock cloud server.
func SetAPIBaseForTest(base string) func() {
	old := apiBase
	apiBase = base
	return func() { apiBase = old }
}

type Session struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Email        string    `json:"email"`
	Babies       []Baby    `json:"babies,omitempty"`
}

type Baby struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	CameraUID string `json:"camera_uid"`
}

type TokenManager struct {
	mu       sync.RWMutex
	session  Session
	password string
	filePath string
	client   *http.Client
}

func NewTokenManager(email, password, sessionFile string) *TokenManager {
	return &TokenManager{
		session:  Session{Email: email},
		password: password,
		filePath: sessionFile,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (tm *TokenManager) SetCredentials(email, password string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.session.Email = email
	tm.password = password
}

func (tm *TokenManager) GetAccessToken() (string, error) {
	tm.mu.RLock()
	token := tm.session.AccessToken
	expires := tm.session.ExpiresAt
	tm.mu.RUnlock()

	if token != "" && time.Now().Before(expires.Add(-tokenRefreshBuffer)) {
		return token, nil
	}

	return tm.refresh()
}

func (tm *TokenManager) GetSession() Session {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.session
}

func (tm *TokenManager) HTTPClient() *http.Client {
	return tm.client
}

// Login performs initial email/password authentication.
// Returns an MFA token if MFA is required.
func (tm *TokenManager) Login() (mfaToken string, err error) {
	body := map[string]string{
		"email":    tm.session.Email,
		"password": tm.password,
	}

	resp, err := tm.apiPost("/login", body)
	if err != nil {
		return "", fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("login: read response: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("login: failed to parse response: %w", err)
	}

	// HTTP 482 or presence of mfa_token means MFA is required.
	if resp.StatusCode == 482 || result["mfa_token"] != nil {
		if tok, ok := result["mfa_token"].(string); ok {
			return tok, nil
		}
		return "", fmt.Errorf("login: MFA required but no mfa_token in response")
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("login failed: HTTP %d", resp.StatusCode)
	}

	return "", tm.parseAuthResponse(data)
}

// LoginWithMFA completes login with the MFA code.
func (tm *TokenManager) LoginWithMFA(mfaToken, mfaCode string) error {
	body := map[string]string{
		"email":     tm.session.Email,
		"password":  tm.password,
		"mfa_token": mfaToken,
		"mfa_code":  mfaCode,
	}

	resp, err := tm.apiPost("/login", body)
	if err != nil {
		return fmt.Errorf("MFA login request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("MFA login: read response: %w", err)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("MFA login failed: HTTP %d", resp.StatusCode)
	}

	return tm.parseAuthResponse(data)
}

func (tm *TokenManager) refresh() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Double-check after acquiring write lock.
	if tm.session.AccessToken != "" && time.Now().Before(tm.session.ExpiresAt.Add(-tokenRefreshBuffer)) {
		return tm.session.AccessToken, nil
	}

	if tm.session.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token available; initial login required")
	}

	body := map[string]string{
		"refresh_token": tm.session.RefreshToken,
	}

	req, err := tm.buildRequest("POST", apiBase+"/tokens/refresh", body)
	if err != nil {
		return "", err
	}
	// Token refresh uses the bare access token (not Bearer), per Nanit API convention.
	if tm.session.AccessToken != "" {
		req.Header.Set("Authorization", tm.session.AccessToken)
	}

	resp, err := tm.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("token refresh: read response: %w", err)
	}

	if resp.StatusCode == 404 {
		return "", fmt.Errorf("refresh token expired; re-login required")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token refresh failed: HTTP %d", resp.StatusCode)
	}

	if err := tm.parseAuthResponseLocked(data); err != nil {
		return "", err
	}

	return tm.session.AccessToken, nil
}

func (tm *TokenManager) FetchBabies() ([]Baby, error) {
	token, err := tm.GetAccessToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", apiBase+"/babies", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch babies failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch babies: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch babies: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Babies []struct {
			UID    string `json:"uid"`
			Name   string `json:"name"`
			Camera struct {
				UID string `json:"uid"`
			} `json:"camera"`
		} `json:"babies"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("fetch babies: parse error: %w", err)
	}

	babies := make([]Baby, len(result.Babies))
	for i, b := range result.Babies {
		babies[i] = Baby{
			UID:       b.UID,
			Name:      b.Name,
			CameraUID: b.Camera.UID,
		}
	}

	tm.mu.Lock()
	tm.session.Babies = babies
	tm.mu.Unlock()

	tm.save()
	return babies, nil
}

// NotificationSettings maps setting keys to enabled/disabled.
type NotificationSettings map[string]bool

// GetNotificationSettings fetches the server-side push notification settings
// for a baby. Each key (e.g. "SOUND", "MOTION") maps to an on/off bool.
func (tm *TokenManager) GetNotificationSettings(babyUID string) (NotificationSettings, error) {
	token, err := tm.GetAccessToken()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/babies/%s/notification_settings", apiBase, babyUID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get notification settings: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("get notification settings: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("get notification settings: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Settings NotificationSettings `json:"settings"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("get notification settings: parse: %w", err)
	}
	return result.Settings, nil
}

// PutNotificationSettings updates one or more notification settings and returns
// the full resulting state.
func (tm *TokenManager) PutNotificationSettings(babyUID string, updates NotificationSettings) (NotificationSettings, error) {
	token, err := tm.GetAccessToken()
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{"settings": updates}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/babies/%s/notification_settings", apiBase, babyUID)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("put notification settings: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("put notification settings: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("put notification settings: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Settings NotificationSettings `json:"settings"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("put notification settings: parse: %w", err)
	}
	return result.Settings, nil
}

// AlertMessage represents a cloud-side detection (SOUND, MOTION, etc.)
type AlertMessage struct {
	ID        int64  `json:"id"`
	BabyUID   string `json:"baby_uid"`
	Type      string `json:"type"`
	Time      int64  `json:"time"`
	CreatedAt string `json:"created_at"`
}

// FetchMessages returns recent alert messages from the Nanit cloud API.
// Pass lastID > 0 to only return messages newer than that ID.
func (tm *TokenManager) FetchMessages(babyUID string, limit int) ([]AlertMessage, error) {
	token, err := tm.GetAccessToken()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/babies/%s/messages?limit=%d", apiBase, babyUID, limit)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch messages: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Messages []AlertMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("fetch messages: parse: %w", err)
	}

	return result.Messages, nil
}

// LoadSession restores a previously saved session from disk.
func (tm *TokenManager) LoadSession() error {
	data, err := os.ReadFile(tm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("corrupt session file: %w", err)
	}

	s.Email = tm.session.Email
	tm.session = s
	return nil
}

func (tm *TokenManager) parseAuthResponse(data []byte) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.parseAuthResponseLocked(data)
}

func (tm *TokenManager) parseAuthResponseLocked(data []byte) error {
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parse auth response: %w", err)
	}

	tm.session.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		tm.session.RefreshToken = result.RefreshToken
	}

	ttl := defaultTokenTTL
	if result.ExpiresIn > 0 {
		ttl = time.Duration(result.ExpiresIn) * time.Second
	}
	tm.session.ExpiresAt = time.Now().Add(ttl)

	tm.saveLocked()
	return nil
}

func (tm *TokenManager) save() {
	tm.mu.RLock()
	sessionCopy := tm.session
	tm.mu.RUnlock()

	data, err := json.MarshalIndent(sessionCopy, "", "  ")
	if err != nil {
		log.Printf("[auth] warning: failed to marshal session: %v", err)
		return
	}
	if err := os.MkdirAll(dirOf(tm.filePath), 0o700); err != nil {
		log.Printf("[auth] warning: failed to create session dir: %v", err)
		return
	}
	if err := os.WriteFile(tm.filePath, data, 0o600); err != nil {
		log.Printf("[auth] warning: failed to write session file: %v", err)
	}
}

func (tm *TokenManager) saveLocked() {
	data, err := json.MarshalIndent(tm.session, "", "  ")
	if err != nil {
		log.Printf("[auth] warning: failed to marshal session: %v", err)
		return
	}
	if err := os.MkdirAll(dirOf(tm.filePath), 0o700); err != nil {
		log.Printf("[auth] warning: failed to create session dir: %v", err)
		return
	}
	if err := os.WriteFile(tm.filePath, data, 0o600); err != nil {
		log.Printf("[auth] warning: failed to write session file: %v", err)
	}
}

func (tm *TokenManager) apiPost(path string, body interface{}) (*http.Response, error) {
	req, err := tm.buildRequest("POST", apiBase+path, body)
	if err != nil {
		return nil, err
	}
	return tm.client.Do(req)
}

func (tm *TokenManager) buildRequest(method, url string, body interface{}) (*http.Request, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

type BmmPatternPoint struct {
	X int
	Y int
}

type bmmBox struct {
	X1 float64 `json:"x1"`
	X2 float64 `json:"x2"`
	Y1 float64 `json:"y1"`
	Y2 float64 `json:"y2"`
}

type bmmObject struct {
	Box   bmmBox  `json:"box"`
	Score float64 `json:"score"`
}

type bmmSessionData struct {
	Objects []bmmObject `json:"objects"`
}

type bmmSession struct {
	BabyUID      string         `json:"baby_uid"`
	CameraStatus string         `json:"camera_status"`
	CameraUID    string         `json:"camera_uid"`
	Detected     bool           `json:"detected"`
	TriggerType  string         `json:"trigger_type"`
	TriggerID    string         `json:"trigger_id"`
	Data         bmmSessionData `json:"data"`
}

type bmmPatternLocationResponse struct {
	BmmSessions bmmSession `json:"bmm_sessions"`
}

// GetBmmPatternLocation uploads a camera frame to the Nanit cloud API for
// baby position detection. Returns the detected pattern centre point or an
// error if detection fails.
func (tm *TokenManager) GetBmmPatternLocation(babyUID string, framePNG []byte, irOn bool) (*BmmPatternPoint, error) {
	token, err := tm.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="image"; filename="image"`)
	h.Set("Content-Type", "image/png")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("create image part: %w", err)
	}
	if _, err := part.Write(framePNG); err != nil {
		return nil, fmt.Errorf("write image: %w", err)
	}

	cameraStatus := "rgb"
	if irOn {
		cameraStatus = "ir"
	}
	if err := w.WriteField("camera_status", cameraStatus); err != nil {
		return nil, fmt.Errorf("write camera_status: %w", err)
	}
	w.Close()

	url := fmt.Sprintf("%s/focus/babies/%s/bmm/sessions", apiBase, babyUID)
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("nanit-api-version", apiVersion)

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bmm request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bmm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bmm status %d: %s", resp.StatusCode, string(body))
	}

	var result bmmPatternLocationResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse bmm response: %w", err)
	}

	session := result.BmmSessions
	if !session.Detected {
		return nil, fmt.Errorf("baby not detected in frame")
	}

	if len(session.Data.Objects) == 0 {
		return nil, fmt.Errorf("no objects returned")
	}

	box := session.Data.Objects[0].Box
	x := int((box.X1 + box.X2) / 2)
	y := int((box.Y1 + box.Y2) / 2)

	return &BmmPatternPoint{X: x, Y: y}, nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
