package nanit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	fcm "github.com/morhaviv/go-fcm-receiver"
)

const (
	nanitFirebaseApiKey    = "AIzaSyDcp8pEeQBxMMGaiNlNIfS10IaDklF_h5E"
	nanitFirebaseAppId     = "1:25705829844:android:83a90cd56c7431e0"
	nanitFirebaseProjectId = "nanit-144706"
)

type PushCredentials struct {
	FcmToken      string `json:"fcm_token"`
	GcmToken      string `json:"gcm_token"`
	AndroidId     uint64 `json:"android_id"`
	SecurityToken uint64 `json:"security_token"`
	PrivateKey    string `json:"private_key"`
	AuthSecret    string `json:"auth_secret"`
	NanitDeviceID int64  `json:"nanit_device_id"`
}

type PushNotification struct {
	Type    string `json:"type"`
	BabyUID string `json:"baby_uid"`
	Time    float64 `json:"time"`
	ID      int64  `json:"id"`
}

type PushReceiver struct {
	mu          sync.Mutex
	creds       *PushCredentials
	credsFile   string
	tokenMgr    *TokenManager
	client      *fcm.FCMClient
	onMessage   func(PushNotification)
	stopCh      chan struct{}
	running     bool
	staleCount  int
}

func NewPushReceiver(tokenMgr *TokenManager, credsFile string) *PushReceiver {
	return &PushReceiver{
		tokenMgr:  tokenMgr,
		credsFile: credsFile,
		stopCh:    make(chan struct{}),
	}
}

func (p *PushReceiver) OnMessage(fn func(PushNotification)) {
	p.onMessage = fn
}

func (p *PushReceiver) Start() error {
	creds, err := p.loadCredentials()
	if err != nil {
		log.Printf("[push] no saved credentials, registering new FCM device...")
		creds, err = p.register()
		if err != nil {
			return fmt.Errorf("FCM registration: %w", err)
		}
	} else {
		log.Printf("[push] loaded saved FCM credentials (android_id: %d)", creds.AndroidId)
	}

	p.mu.Lock()
	p.creds = creds
	p.mu.Unlock()

	if creds.NanitDeviceID == 0 {
		if err := p.registerWithNanit(creds); err != nil {
			return fmt.Errorf("register push token with Nanit: %w", err)
		}
	}

	go p.listenLoop()
	return nil
}

func (p *PushReceiver) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		close(p.stopCh)
		if p.client != nil {
			p.client.Close()
		}
	}
}

func (p *PushReceiver) register() (*PushCredentials, error) {
	client := &fcm.FCMClient{
		ApiKey:    nanitFirebaseApiKey,
		AppId:     nanitFirebaseAppId,
		ProjectID: nanitFirebaseProjectId,
		AndroidApp: &fcm.AndroidFCM{
			GcmSenderId:        "25705829844",
			AndroidPackage:     "com.nanit.baby",
			AndroidPackageCert: "5099ace03019d88c8115fa90a37b2ed014bbea42",
		},
	}

	privateKey, authSecret, err := client.CreateNewKeys()
	if err != nil {
		return nil, fmt.Errorf("create keys: %w", err)
	}

	fcmToken, gcmToken, androidId, securityToken, err := client.Register()
	if err != nil {
		return nil, fmt.Errorf("FCM register: %w", err)
	}

	creds := &PushCredentials{
		FcmToken:      fcmToken,
		GcmToken:      gcmToken,
		AndroidId:     androidId,
		SecurityToken: securityToken,
		PrivateKey:    privateKey,
		AuthSecret:    authSecret,
	}

	if err := p.saveCredentials(creds); err != nil {
		log.Printf("[push] warning: could not save credentials: %v", err)
	}

	log.Printf("[push] registered new FCM device (android_id: %d)", androidId)
	return creds, nil
}

func (p *PushReceiver) registerWithNanit(creds *PushCredentials) error {
	token, err := p.tokenMgr.GetAccessToken()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	pushToken := creds.FcmToken
	if pushToken == "" {
		pushToken = creds.GcmToken
	}
	body := map[string]string{"token": pushToken}
	bodyBytes, _ := json.Marshal(body)

	url := apiBase + "/devices/android"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	req.Header.Set("nanit-api-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /devices/android: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[push] Nanit registration failed: HTTP %d body=%s", resp.StatusCode, string(respBody))
		return fmt.Errorf("POST /devices/android returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	creds.NanitDeviceID = result.Device.ID
	if err := p.saveCredentials(creds); err != nil {
		log.Printf("[push] warning: could not save updated credentials: %v", err)
	}

	log.Printf("[push] registered FCM token with Nanit (device_id: %d)", creds.NanitDeviceID)
	return nil
}

func (p *PushReceiver) listenLoop() {
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		if err := p.listen(); err != nil {
			log.Printf("[push] FCM listener error: %v, reconnecting in 5s...", err)
			select {
			case <-p.stopCh:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (p *PushReceiver) listen() error {
	p.mu.Lock()
	creds := p.creds
	p.mu.Unlock()

	client := &fcm.FCMClient{
		ApiKey:        nanitFirebaseApiKey,
		AppId:         nanitFirebaseAppId,
		ProjectID:     nanitFirebaseProjectId,
		GcmToken:      creds.GcmToken,
		FcmToken:      creds.FcmToken,
		AndroidId:     creds.AndroidId,
		SecurityToken: creds.SecurityToken,
		OnDataMessage: p.handleEncryptedMessage,
		OnRawMessage:  p.handleRawMessage,
		AndroidApp: &fcm.AndroidFCM{
			GcmSenderId:        "25705829844",
			AndroidPackage:     "com.nanit.baby",
			AndroidPackageCert: "5099ace03019d88c8115fa90a37b2ed014bbea42",
		},
	}

	if err := client.LoadKeys(creds.PrivateKey, creds.AuthSecret); err != nil {
		return fmt.Errorf("load keys: %w", err)
	}

	p.mu.Lock()
	p.client = client
	p.mu.Unlock()

	log.Printf("[push] FCM listener connected, waiting for notifications...")
	return client.StartListening()
}

func (p *PushReceiver) handleEncryptedMessage(message []byte) {
	log.Printf("[push] received encrypted notification: %s", string(message))
	p.parseAndDispatch(message)
}

func (p *PushReceiver) handleRawMessage(message *fcm.DataMessageStanza) {
	notifData := ""
	for _, appData := range message.AppData {
		if appData.GetKey() == "notification" {
			notifData = appData.GetValue()
			break
		}
	}

	if notifData == "" {
		log.Printf("[push] received raw message without notification data (from: %s)", message.GetFrom())
		return
	}

	p.parseAndDispatch([]byte(notifData))
}

func (p *PushReceiver) parseAndDispatch(data []byte) {
	var notif PushNotification
	if err := json.Unmarshal(data, &notif); err != nil {
		log.Printf("[push] failed to parse notification: %v (raw: %s)", err, string(data))
		return
	}

	if notif.Type == "" {
		var wrapper struct {
			Notification json.RawMessage `json:"notification"`
		}
		if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Notification != nil {
			if err := json.Unmarshal(wrapper.Notification, &notif); err != nil {
				log.Printf("[push] failed to parse inner notification: %v", err)
				return
			}
		}
	}

	if notif.Type == "" || p.onMessage == nil {
		return
	}

	age := time.Since(time.Unix(int64(notif.Time), 0))
	if age > 60*time.Second {
		p.staleCount++
		return
	}

	if p.staleCount > 0 {
		log.Printf("[push] dropped %d stale buffered notifications", p.staleCount)
		p.staleCount = 0
	}

	log.Printf("[push] %s for %s", notif.Type, notif.BabyUID)
	p.onMessage(notif)
}

func (p *PushReceiver) loadCredentials() (*PushCredentials, error) {
	data, err := os.ReadFile(p.credsFile)
	if err != nil {
		return nil, err
	}
	var creds PushCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}
	if creds.AndroidId == 0 || creds.FcmToken == "" {
		return nil, fmt.Errorf("incomplete credentials")
	}
	return &creds, nil
}

func (p *PushReceiver) saveCredentials(creds *PushCredentials) error {
	if err := os.MkdirAll(filepath.Dir(p.credsFile), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.credsFile, data, 0600)
}
