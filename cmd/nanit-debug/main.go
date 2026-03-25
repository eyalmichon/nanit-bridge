package main

import (
	"bufio"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"nanit-bridge/internal/nanit"
	pb "nanit-bridge/internal/nanit/nanitpb"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[debug] ")

	email := os.Getenv("NANIT_EMAIL")
	password := os.Getenv("NANIT_PASSWORD")
	sessionFile := os.Getenv("NANIT_SESSION_FILE")
	if sessionFile == "" {
		sessionFile = "/tmp/nanit/session.json"
	}

	tokenMgr := nanit.NewTokenManager(email, password, sessionFile)

	if err := tokenMgr.LoadSession(); err != nil {
		log.Printf("no saved session: %v", err)
	}

	if err := ensureAuth(tokenMgr, email, password); err != nil {
		log.Fatalf("auth: %v", err)
	}

	babies, err := tokenMgr.FetchBabies()
	if err != nil {
		log.Fatalf("fetch babies: %v", err)
	}

	log.Printf("found %d baby/camera(s)", len(babies))
	for _, b := range babies {
		log.Printf("  %s — camera %s (%s)", b.UID, b.CameraUID, b.Name)
	}

	if len(babies) == 0 {
		log.Fatal("no cameras to monitor")
	}

	camera := babies[0]
	log.Printf("connecting to camera %s ...", camera.CameraUID)

	token, err := tokenMgr.GetAccessToken()
	if err != nil {
		log.Fatalf("get token: %v", err)
	}

	url := fmt.Sprintf("wss://api.nanit.com/focus/cameras/%s/user_connect", camera.CameraUID)
	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{},
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(url, http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if err != nil {
		log.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()

	log.Printf("connected! dumping all messages...\n")

	// Enable ALL sensor data transfer
	sendDebugRequest(conn, 1, pb.RequestType_PUT_CONTROL, func(req *pb.Request) {
		t := true
		req.Control = &pb.Control{
			SensorDataTransfer: &pb.Control_SensorDataTransfer{
				Sound:       &t,
				Motion:      &t,
				Temperature: &t,
				Humidity:    &t,
				Light:       &t,
				Night:       &t,
			},
		}
	})

	// GET_CONTROL to verify sensor_data_transfer state
	sendDebugRequest(conn, 2, pb.RequestType_GET_CONTROL, func(req *pb.Request) {
		t := true
		req.GetControl_ = &pb.GetControl{
			NightLight:           &t,
			NightLightTimeout:    &t,
			SensorDataTransferEn: &t,
		}
	})

	sendDebugRequest(conn, 3, pb.RequestType_GET_SENSOR_DATA, func(req *pb.Request) {
		all := true
		req.GetSensorData = &pb.GetSensorData{All: &all}
	})

	sendDebugRequest(conn, 4, pb.RequestType_GET_SETTINGS, nil)
	sendDebugRequest(conn, 5, pb.RequestType_GET_STATUS, func(req *pb.Request) {
		all := true
		req.GetStatus_ = &pb.GetStatus{All: &all}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				log.Printf("read error: %v", err)
				return
			}
			dumpMessage(data)
		}
	}()

	// Keepalive
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				msg := &pb.Message{Type: pb.Message_KEEPALIVE.Enum()}
				data, _ := proto.Marshal(msg)
				if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sig:
		log.Println("shutting down...")
	case <-done:
	}
}

func sendDebugRequest(conn *websocket.Conn, id int32, reqType pb.RequestType, populate func(*pb.Request)) {
	req := &pb.Request{Id: &id, Type: &reqType}
	if populate != nil {
		populate(req)
	}
	msg := &pb.Message{
		Type:    pb.Message_REQUEST.Enum(),
		Request: req,
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		log.Printf("marshal error for %v: %v", reqType, err)
		return
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		log.Printf("send error for %v: %v", reqType, err)
	}
	log.Printf(">>> SENT %v (id=%d)", reqType, id)
}

func dumpMessage(data []byte) {
	msg := &pb.Message{}
	if err := proto.Unmarshal(data, msg); err != nil {
		log.Printf("<<< UNPARSEABLE (%d bytes):\n%s", len(data), hex.Dump(data))
		return
	}

	switch msg.GetType() {
	case pb.Message_KEEPALIVE:
		return // silent

	case pb.Message_REQUEST:
		req := msg.GetRequest()
		if req == nil {
			return
		}
		fmt.Printf("\n╔══ PUSH REQUEST: %v ══════════════════════════\n", req.GetType())
		fmt.Printf("║ Full proto:\n%s", indent(prototext.Format(req)))
		dumpUnknown("Request", req)
		dumpSubfieldUnknowns(req)
		fmt.Printf("╚═══════════════════════════════════════════════\n")

	case pb.Message_RESPONSE:
		resp := msg.GetResponse()
		if resp == nil {
			return
		}
		fmt.Printf("\n┌── RESPONSE: %v (status=%d %s) ─────────────\n",
			resp.GetRequestType(), resp.GetStatusCode(), resp.GetStatusMessage())
		fmt.Printf("│ Full proto:\n%s", indent(prototext.Format(resp)))
		dumpUnknown("Response", resp)
		dumpSubfieldUnknownsResp(resp)
		fmt.Printf("└───────────────────────────────────────────────\n")
	}
}

func dumpUnknown(label string, m proto.Message) {
	unknown := m.ProtoReflect().GetUnknown()
	if len(unknown) > 0 {
		fmt.Printf("│ ⚠ %s has %d unknown bytes:\n%s", label, len(unknown), indent(hex.Dump(unknown)))
	}
}

func dumpSubfieldUnknowns(req *pb.Request) {
	if p := req.GetPlayback(); p != nil {
		dumpUnknown("Request.Playback", p)
	}
	if s := req.GetSettings(); s != nil {
		dumpUnknown("Request.Settings", s)
	}
	if c := req.GetControl(); c != nil {
		dumpUnknown("Request.Control", c)
		if sdt := c.GetSensorDataTransfer(); sdt != nil {
			dumpUnknown("Request.Control.SensorDataTransfer", sdt)
		}
	}
	if st := req.GetStreaming(); st != nil {
		dumpUnknown("Request.Streaming", st)
	}
	for i, sd := range req.GetSensorData_() {
		unknown := sd.ProtoReflect().GetUnknown()
		if len(unknown) > 0 {
			fmt.Printf("│ ⚠ SensorData[%d] has %d unknown bytes:\n%s",
				i, len(unknown), indent(hex.Dump(unknown)))
		}
	}
}

func dumpSubfieldUnknownsResp(resp *pb.Response) {
	if p := resp.GetPlayback(); p != nil {
		dumpUnknown("Response.Playback", p)
	}
	if s := resp.GetSettings(); s != nil {
		dumpUnknown("Response.Settings", s)
	}
	if c := resp.GetControl(); c != nil {
		dumpUnknown("Response.Control", c)
	}
	for i, sd := range resp.GetSensorData() {
		unknown := sd.ProtoReflect().GetUnknown()
		if len(unknown) > 0 {
			fmt.Printf("│ ⚠ SensorData[%d] has %d unknown bytes:\n%s",
				i, len(unknown), indent(hex.Dump(unknown)))
		}
	}
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "│   " + l
		}
	}
	return strings.Join(lines, "\n")
}

func ensureAuth(tokenMgr *nanit.TokenManager, email, password string) error {
	session := tokenMgr.GetSession()
	if session.RefreshToken != "" {
		_, err := tokenMgr.GetAccessToken()
		if err == nil {
			log.Println("authenticated using saved session")
			return nil
		}
		log.Printf("saved session expired: %v", err)
	}

	if email == "" || password == "" {
		return fmt.Errorf("no valid session and NANIT_EMAIL/NANIT_PASSWORD not set")
	}

	mfaToken, err := tokenMgr.Login()
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	if mfaToken != "" {
		fmt.Print("Enter MFA code: ")
		reader := bufio.NewReader(os.Stdin)
		code, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading MFA code: %w", err)
		}
		if err := tokenMgr.LoginWithMFA(mfaToken, strings.TrimSpace(code)); err != nil {
			return fmt.Errorf("MFA: %w", err)
		}
	}

	log.Println("authenticated successfully")
	return nil
}
