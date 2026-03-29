package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"nanit-bridge/internal/api"
	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/config"
	"nanit-bridge/internal/mqtt"
	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--reset-dashboard-password" {
			resetDashboardPassword()
			return
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[nanit-bridge] ")

	logBcast := api.NewLogBroadcaster()
	log.SetOutput(logBcast)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if _, err := os.Stat(cfg.DashboardAuthFile); os.IsNotExist(err) && cfg.DashboardPassword != "" {
		if err := writeDashboardPasswordHash(cfg.DashboardAuthFile, cfg.DashboardPassword); err != nil {
			log.Fatalf("failed to initialize dashboard password from NANIT_DASHBOARD_PASSWORD: %v", err)
		}
		log.Printf("initialized dashboard password hash at %s from NANIT_DASHBOARD_PASSWORD", cfg.DashboardAuthFile)
	}

	tokenMgr := nanit.NewTokenManager(cfg.NanitEmail, cfg.NanitPassword, cfg.SessionFile)

	if err := tokenMgr.LoadSession(); err != nil {
		log.Printf("warning: could not load session: %v", err)
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

	mgr := baby.NewManager(tokenMgr, cfg.RTMPAddr, cfg.SensorPollSec, cfg.PushCredsFile, rtmpServer)

	rtmpServer.OnPublisherDisconnect(func(streamKey string) {
		mgr.RestartStream(streamKey)
	})

	startOrRestartManager := func() error {
		if mgr.IsStarted() {
			if err := mgr.Restart(); err != nil {
				return fmt.Errorf("restart manager: %w", err)
			}
		} else if err := mgr.Start(); err != nil {
			return fmt.Errorf("start manager: %w", err)
		}

		if mqttPub != nil {
			for uid, state := range mgr.AllStates() {
				mqttPub.PublishDiscovery(uid, state.Name)
			}
		}
		return nil
	}

	apiServer := api.NewServer(
		cfg.HTTPPort,
		mgr,
		rtmpServer,
		logBcast,
		cfg.DashboardAuthFile,
		tokenMgr,
		startOrRestartManager,
	)

	mgr.OnStateChange(func(babyUID string, state *baby.State) {
		if mqttPub != nil {
			mqttPub.PublishState(babyUID, state)
		}
		apiServer.BroadcastState(babyUID, state)
	})

	if err := apiServer.Start(); err != nil {
		log.Fatalf("api server: %v", err)
	}

	if err := ensureAuth(tokenMgr, cfg, term.IsTerminal(int(syscall.Stdin))); err != nil {
		log.Printf("nanit cloud auth pending: %v", err)
		log.Printf("dashboard is available; connect via /settings or the dashboard auth modal")
	} else {
		if err := startOrRestartManager(); err != nil {
			log.Printf("nanit authenticated but manager start failed: %v", err)
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

func ensureAuth(tokenMgr *nanit.TokenManager, cfg *config.Config, interactive bool) error {
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

	email := cfg.NanitEmail
	password := cfg.NanitPassword

	if email == "" || password == "" {
		if !interactive {
			return fmt.Errorf("no valid session and NANIT_EMAIL/NANIT_PASSWORD not set")
		}
		reader := bufio.NewReader(os.Stdin)
		if email == "" {
			fmt.Print("Nanit email: ")
			line, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading email: %w", err)
			}
			email = strings.TrimSpace(line)
		}
		if password == "" {
			fmt.Print("Nanit password: ")
			pw, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("reading password: %w", err)
			}
			password = string(pw)
		}
		tokenMgr.SetCredentials(email, password)
	}

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

func resetDashboardPassword() {
	_ = config.LoadEnvFile()

	authFile := os.Getenv("NANIT_DASHBOARD_AUTH_FILE")
	if authFile == "" {
		authFile = "/data/dashboard_password.hash"
	}

	fmt.Print("New dashboard password: ")
	pw1, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading password: %v\n", err)
		os.Exit(1)
	}
	if len(pw1) == 0 {
		fmt.Fprintln(os.Stderr, "password cannot be empty")
		os.Exit(1)
	}

	fmt.Print("Confirm password: ")
	pw2, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading password: %v\n", err)
		os.Exit(1)
	}

	if string(pw1) != string(pw2) {
		fmt.Fprintln(os.Stderr, "passwords do not match")
		os.Exit(1)
	}

	if err := writeDashboardPasswordHash(authFile, string(pw1)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", authFile, err)
		os.Exit(1)
	}

	fmt.Printf("Dashboard password saved to %s\n", authFile)
}

func writeDashboardPasswordHash(authFile, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bcrypt error: %w", err)
	}

	if dir := filepath.Dir(authFile); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := os.WriteFile(authFile, hash, 0o600); err != nil {
		return err
	}

	return nil
}
