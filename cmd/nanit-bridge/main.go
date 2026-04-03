package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"nanit-bridge/internal/api"
	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/config"
	"nanit-bridge/internal/mqtt"
	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
)

var version string

var (
	flagVersion = flag.Bool("version", false, "print version and exit")
	flagHealth  = flag.Bool("healthcheck", false, "run Docker healthcheck and exit")
	flagResetPW = flag.Bool("reset-dashboard-password", false, "interactively reset the dashboard password")
)

func main() {
	flag.Parse()

	if version == "" {
		version = vcsVersion()
	}

	if *flagVersion {
		fmt.Println("nanit-bridge " + version)
		return
	}
	if *flagHealth {
		runHealthcheck()
		return
	}
	if *flagResetPW {
		resetDashboardPassword()
		return
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

	rtmpServer := rtmp.NewServer(cfg.RTMPPort, cfg.RTMPToken)
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

	mgr := baby.NewManager(tokenMgr, cfg.RTMPAddr, cfg.RTMPToken, cfg.SensorPollSec, cfg.PushCredsFile, rtmpServer)

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
		cfg.RTMPAddr,
		cfg.RTMPTokenFile,
		version,
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

	if err := ensureAuth(tokenMgr, cfg, term.IsTerminal(int(syscall.Stdin)), apiServer.SetPendingMFA); err != nil {
		log.Printf("nanit cloud auth pending: %v", err)
		log.Printf("open the dashboard at http://0.0.0.0:%d to complete authentication", cfg.HTTPPort)
	} else {
		if err := startOrRestartManager(); err != nil {
			log.Printf("nanit authenticated but manager start failed: %v", err)
		}
	}

	log.Printf("nanit-bridge is running")
	log.Printf("  RTMP: rtmp://%s/%s/<baby_uid>", cfg.RTMPAddr, maskToken(cfg.RTMPToken))
	log.Printf("  Dashboard: http://0.0.0.0:%d", cfg.HTTPPort)
	if cfg.MQTTBrokerURL != "" {
		log.Printf("  MQTT: %s (prefix: %s)", cfg.MQTTBrokerURL, cfg.MQTTPrefix)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	go func() { <-sig; log.Println("forced exit"); os.Exit(1) }()

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := apiServer.Stop(ctx); err != nil {
		log.Printf("api server shutdown error: %v", err)
	}
	mgr.Stop()
	rtmpServer.Stop()
}

func ensureAuth(tokenMgr *nanit.TokenManager, cfg *config.Config, interactive bool, setMFA func(string)) error {
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
		if !interactive {
			setMFA(mfaToken)
			return fmt.Errorf("MFA required — enter code on dashboard")
		}
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

func runHealthcheck() {
	port := os.Getenv("NANIT_HTTP_PORT")
	if port == "" {
		port = "8080"
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

func vcsVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return rev + dirty
}

func maskToken(s string) string {
	if len(s) <= 12 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}
