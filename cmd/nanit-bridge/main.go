package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"nanit-bridge/internal/api"
	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/config"
	"nanit-bridge/internal/mqtt"
	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[nanit-bridge] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	tokenMgr := nanit.NewTokenManager(cfg.NanitEmail, cfg.NanitPassword, cfg.SessionFile)

	if err := tokenMgr.LoadSession(); err != nil {
		log.Printf("warning: could not load session: %v", err)
	}

	if err := ensureAuth(tokenMgr, cfg); err != nil {
		log.Fatalf("authentication failed: %v", err)
	}

	rtmpServer := rtmp.NewServer(cfg.RTMPPort)
	if err := rtmpServer.Start(); err != nil {
		log.Fatalf("rtmp server: %v", err)
	}

	var mqttPub *mqtt.Publisher
	if cfg.MQTTBrokerURL != "" {
		mqttPub, err = mqtt.NewPublisher(mqtt.Config{
			BrokerURL: cfg.MQTTBrokerURL,
			Username:  cfg.MQTTUsername,
			Password:  cfg.MQTTPassword,
			Prefix:    cfg.MQTTPrefix,
		})
		if err != nil {
			log.Fatalf("mqtt: %v", err)
		}
		defer mqttPub.Close()
		log.Printf("MQTT connected to %s", cfg.MQTTBrokerURL)
	}

	mgr := baby.NewManager(tokenMgr, cfg.RTMPAddr)

	apiServer := api.NewServer(cfg.HTTPPort, mgr, rtmpServer)

	mgr.OnStateChange(func(babyUID string, state *baby.State) {
		if mqttPub != nil {
			mqttPub.PublishState(babyUID, state)
		}
		apiServer.BroadcastState(babyUID, state)
	})

	if err := mgr.Start(); err != nil {
		log.Fatalf("baby manager: %v", err)
	}

	if err := apiServer.Start(); err != nil {
		log.Fatalf("api server: %v", err)
	}

	if mqttPub != nil {
		for uid, state := range mgr.AllStates() {
			mqttPub.PublishDiscovery(uid, state.Name)
		}
	}

	log.Printf("nanit-bridge is running")
	log.Printf("  RTMP: rtmp://%s/local/<baby_uid>", cfg.RTMPAddr)
	log.Printf("  Dashboard: http://0.0.0.0:%d", cfg.HTTPPort)
	if cfg.MQTTBrokerURL != "" {
		log.Printf("  MQTT: %s (prefix: %s)", cfg.MQTTBrokerURL, cfg.MQTTPrefix)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	mgr.Stop()
}

func ensureAuth(tokenMgr *nanit.TokenManager, cfg *config.Config) error {
	// Try refreshing existing session.
	session := tokenMgr.GetSession()
	if session.RefreshToken != "" {
		_, err := tokenMgr.GetAccessToken()
		if err == nil {
			log.Println("authenticated using saved session")
			return nil
		}
		log.Printf("saved session expired: %v", err)
	}

	if cfg.NanitEmail == "" || cfg.NanitPassword == "" {
		return fmt.Errorf("no valid session and NANIT_EMAIL/NANIT_PASSWORD not set")
	}

	// Fresh login required.
	mfaToken, err := tokenMgr.Login()
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	if mfaToken != "" {
		fmt.Print("Enter MFA code from your phone: ")
		reader := bufio.NewReader(os.Stdin)
		code, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading MFA code: %w", err)
		}
		code = strings.TrimSpace(code)

		if err := tokenMgr.LoginWithMFA(mfaToken, code); err != nil {
			return fmt.Errorf("MFA login: %w", err)
		}
	}

	log.Println("authenticated successfully")
	return nil
}
