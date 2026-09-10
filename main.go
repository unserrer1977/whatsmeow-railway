package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3" // register sqlite3 driver
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// ─── State ──────────────────────────────────────────────────────────────────

var (
	client    *whatsmeow.Client
	qrMutex   sync.Mutex
	currentQR string
	connected bool
	apiKey    string
)

// ─── Types ──────────────────────────────────────────────────────────────────

type SendRequest struct {
	Phone   string `json:"phone"`   // E.164 format (for private chats)
	GroupID string `json:"groupId"` // WhatsApp group JID (for group messages)
	Message string `json:"message"` // Text body
	Mentions []string `json:"mentions"` // digits-only phone numbers to @tag (groups)
}

type SendResponse struct {
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	MessageID string `json:"messageId,omitempty"`
}

type QRResponse struct {
	QR        string `json:"qr,omitempty"`
	Connected bool   `json:"connected"`
}

type StatusResponse struct {
	Connected bool   `json:"connected"`
	Phone     string `json:"phone,omitempty"`
}

type GroupInfo struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	Owner       string `json:"owner,omitempty"`
	ParticipantCount int `json:"participantCount"`
	IsAnnounce  bool   `json:"isAnnounce"`
	IsLocked    bool   `json:"isLocked"`
}

type GroupListResponse struct {
	Groups []GroupInfo `json:"groups"`
}

type IncomingMessage struct {
	From        string `json:"from"`
	FromName    string `json:"fromName,omitempty"`
	Chat        string `json:"chat,omitempty"`
	IsGroup     bool   `json:"isGroup"`
	Sender      string `json:"sender,omitempty"`
	Message     string `json:"message"`
	Timestamp   int64  `json:"timestamp"`
	MessageID   string `json:"messageId"`
}

// ─── Main ───────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// API key for authentication (optional but recommended)
	apiKey = os.Getenv("API_KEY")
	if apiKey == "" {
		log.Println("WARNING: API_KEY not set — API endpoints are unauthenticated")
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("Failed to create data dir %s: %v", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "whatsmeow.db")
	log.Printf("Using session database at: %s", dbPath)

	container, err := sqlstore.New(
		context.Background(),
		"sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on", dbPath),
		waLog.Stdout("db", "INFO", true),
	)
	if err != nil {
		log.Fatalf("Failed to create session store: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatalf("Failed to get device: %v", err)
	}

	clientLog := waLog.Stdout("client", "INFO", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(eventHandler)

	// Connect in background with auto-reconnect
	go connectLoop()

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/qr", qrHandler)
	mux.HandleFunc("/status", statusHandler)

	// Authenticated endpoints
	mux.HandleFunc("/api/send", authMiddleware(sendHandler))
	mux.HandleFunc("/api/groups", authMiddleware(groupsHandler))
	mux.HandleFunc("/api/group/", authMiddleware(groupInfoHandler))
	mux.HandleFunc("/api/send-group", authMiddleware(sendGroupHandler))
	mux.HandleFunc("/api/contacts", authMiddleware(contactsHandler))
	mux.HandleFunc("/api/revoke", authMiddleware(revokeHandler))

	// Info endpoint (no auth)
	mux.HandleFunc("/", rootHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		if client != nil && client.IsConnected() {
			client.Disconnect()
		}
		os.Exit(0)
	}()

	log.Printf("HTTP server starting on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// ─── Auth ───────────────────────────────────────────────────────────────────

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey != "" {
			provided := r.Header.Get("X-API-Key")
			if provided == "" {
				// Also accept Bearer token
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					provided = strings.TrimPrefix(auth, "Bearer ")
				}
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// ─── Connection ─────────────────────────────────────────────────────────────

func connectLoop() {
	for {
		if client.Store.ID != nil {
			if !client.IsConnected() {
				log.Println("Session found, connecting to WhatsApp...")
				err := client.Connect()
				if err != nil {
					log.Printf("Failed to connect: %v — retrying in 5s", err)
					time.Sleep(5 * time.Second)
					continue
				}
			}
			time.Sleep(5 * time.Second)
			continue
		}

		if client.IsConnected() {
			client.Disconnect()
		}

		log.Println("No session found. Generating QR code for pairing...")
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Printf("Failed to get QR channel: %v — retrying in 3s", err)
			client.Disconnect()
			time.Sleep(3 * time.Second)
			continue
		}

		qrDone := make(chan bool, 1)
		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					qrMutex.Lock()
					currentQR = evt.Code
					qrMutex.Unlock()
					log.Printf("QR CODE: %s", evt.Code)
				} else {
					log.Printf("QR channel event: %s", evt.Event)
					if evt.Event == "timeout" || evt.Event == "error" {
						qrDone <- true
						return
					}
				}
			}
			qrDone <- true
		}()

		if err := client.Connect(); err != nil {
			log.Printf("Failed to connect for QR: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}

		<-qrDone

		if client.Store.ID != nil {
			log.Println("Paired! Switching to connected mode.")
			qrMutex.Lock()
			currentQR = ""
			qrMutex.Unlock()
			continue
		}

		log.Println("QR timed out. Reconnecting to generate fresh codes...")
		client.Disconnect()
		qrMutex.Lock()
		currentQR = ""
		qrMutex.Unlock()
		time.Sleep(2 * time.Second)
	}
}

// ─── Event Handler ──────────────────────────────────────────────────────────

func eventHandler(evt any) {
	switch v := evt.(type) {
	case *events.PairSuccess:
		qrMutex.Lock()
		currentQR = ""
		connected = true
		qrMutex.Unlock()
		log.Printf("Paired successfully! Phone: %s, ID: %s", v.ID.String(), v.ID.User)
	case *events.Connected:
		qrMutex.Lock()
		connected = true
		qrMutex.Unlock()
		log.Println("Connected to WhatsApp!")
	case *events.Disconnected:
		qrMutex.Lock()
		connected = false
		qrMutex.Unlock()
		log.Println("Disconnected from WhatsApp — will auto-reconnect")
	case *events.Message:
		handleIncomingMessage(v)
	}
}

func handleIncomingMessage(v *events.Message) {
	sender := ""
	if !v.Info.Sender.IsEmpty() {
		sender = v.Info.Sender.String()
	}

	conversation := ""
	if conv := v.Message.GetConversation(); conv != "" {
		conversation = conv
	}

	isGroup := v.Info.Chat.Server == types.GroupServer
	chat := v.Info.Chat.String()

	log.Printf("Received message from %s in %s (group=%v): %s", sender, chat, isGroup, conversation)

	// Log as JSON for external parsing from Railway logs
	if conversation != "" {
		msg := IncomingMessage{
			From:      chat,
			Chat:      chat,
			IsGroup:   isGroup,
			Sender:    sender,
			Message:   conversation,
			Timestamp: v.Info.Timestamp.Unix(),
			MessageID: v.Info.ID,
		}
		jsonBytes, _ := json.Marshal(msg)
		log.Printf("INCOMING_MESSAGE_JSON: %s", string(jsonBytes))
	}
}

// ─── HTTP Handlers ──────────────────────────────────────────────────────────

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "whatsmeow",
	})
}

func qrHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	qrMutex.Lock()
	qr := currentQR
	conn := connected
	qrMutex.Unlock()
	json.NewEncoder(w).Encode(QRResponse{QR: qr, Connected: conn})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	qrMutex.Lock()
	conn := connected
	qrMutex.Unlock()
	phone := ""
	if client != nil && client.Store.ID != nil {
		phone = client.Store.ID.User
	}
	json.NewEncoder(w).Encode(StatusResponse{Connected: conn, Phone: phone})
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(SendResponse{Error: "POST required"})
		return
	}

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(SendResponse{Error: "WhatsApp not connected"})
		return
	}

	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "Invalid JSON: " + err.Error()})
		return
	}

	if req.Message == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "message required"})
		return
	}

	if req.Phone == "" && req.GroupID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "either phone or groupId required"})
		return
	}

	var jid types.JID
	var err error

	if req.GroupID != "" {
		jid, err = types.ParseJID(req.GroupID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendResponse{Error: "invalid groupId: " + err.Error()})
			return
		}
		if jid.Server != types.GroupServer {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendResponse{Error: "groupId is not a group JID"})
			return
		}
	} else {
		jid, err = parsePhoneToJID(req.Phone)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendResponse{Error: err.Error()})
			return
		}
	}

	resp, err := client.SendMessage(r.Context(), jid, &waE2E.Message{
		Conversation: &req.Message,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(SendResponse{Error: "Send failed: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(SendResponse{
		Success:   true,
		MessageID: string(resp.ID),
	})
}

// Alias for group sending — same logic but groupId is required
func sendGroupHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(SendResponse{Error: "POST required"})
		return
	}

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(SendResponse{Error: "WhatsApp not connected"})
		return
	}

	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "Invalid JSON: " + err.Error()})
		return
	}

	if req.GroupID == "" || req.Message == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "groupId and message required"})
		return
	}

	jid, err := types.ParseJID(req.GroupID)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "invalid groupId: " + err.Error()})
		return
	}

	if jid.Server != types.GroupServer {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "not a group JID"})
		return
	}

	msg := &waE2E.Message{
		Conversation: proto.String(req.Message),
	}

	// If mentions were supplied, send as an ExtendedTextMessage carrying
	// ContextInfo.MentionedJID so WhatsApp renders real @mentions.
	if len(req.Mentions) > 0 {
		jids := make([]string, 0, len(req.Mentions))
		for _, m := range req.Mentions {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, m)
			if digits == "" {
				continue
			}
			jids = append(jids, types.NewJID(digits, types.DefaultUserServer).String())
		}
		if len(jids) > 0 {
			msg = &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text:        proto.String(req.Message),
					ContextInfo: &waE2E.ContextInfo{MentionedJID: jids},
				},
			}
		}
	}

	resp, err := client.SendMessage(r.Context(), jid, msg)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(SendResponse{Error: "Send failed: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(SendResponse{
		Success:   true,
		MessageID: string(resp.ID),
	})
}

func groupsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "WhatsApp not connected"})
		return
	}

	groups, err := client.GetJoinedGroups(context.Background())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to list groups: " + err.Error()})
		return
	}

	var result []GroupInfo
	for _, g := range groups {
		info := GroupInfo{
			JID:             g.JID.String(),
			Name:            g.Name,
			ParticipantCount: len(g.Participants),
			IsAnnounce:      g.IsAnnounce,
			IsLocked:        g.IsLocked,
		}
		if !g.OwnerJID.IsEmpty() {
			info.Owner = g.OwnerJID.String()
		}
		result = append(result, info)
	}

	if result == nil {
		result = []GroupInfo{}
	}

	json.NewEncoder(w).Encode(GroupListResponse{Groups: result})
}

func groupInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract group JID from path: /api/group/{jid}
	parts := strings.SplitN(r.URL.Path, "/", 4)
	if len(parts) < 4 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "group JID required in path"})
		return
	}
	groupJIDStr := parts[3]

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "WhatsApp not connected"})
		return
	}

	jid, err := types.ParseJID(groupJIDStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid group JID"})
		return
	}

	info, err := client.GetGroupInfo(context.Background(), jid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to get group info: " + err.Error()})
		return
	}

	result := GroupInfo{
		JID:             info.JID.String(),
		Name:            info.Name,
		ParticipantCount: len(info.Participants),
		IsAnnounce:      info.IsAnnounce,
		IsLocked:        info.IsLocked,
	}
	if !info.OwnerJID.IsEmpty() {
		result.Owner = info.OwnerJID.String()
	}

	json.NewEncoder(w).Encode(result)
}

func contactsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "WhatsApp not connected"})
		return
	}

	// Fetch contacts from app state
	contacts, err := client.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to get contacts: " + err.Error()})
		return
	}

	type ContactInfo struct {
		JID  string `json:"jid"`
		Name string `json:"name"`
	}

	var result []ContactInfo
	for jid, contact := range contacts {
		name := contact.FullName
		if name == "" {
			name = contact.PushName
		}
		if name == "" {
			name = jid.User
		}
		result = append(result, ContactInfo{
			JID:  jid.String(),
			Name: name,
		})
	}

	if result == nil {
		result = []ContactInfo{}
	}

	json.NewEncoder(w).Encode(map[string]any{"contacts": result})
}

func revokeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "POST required"})
		return
	}

	if !isConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "WhatsApp not connected"})
		return
	}

	var req struct {
		Phone     string `json:"phone"`
		GroupID   string `json:"groupId"`
		MessageID string `json:"messageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.MessageID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "messageId required"})
		return
	}

	var jid types.JID
	var err error
	if req.GroupID != "" {
		jid, err = types.ParseJID(req.GroupID)
	} else if req.Phone != "" {
		jid, err = parsePhoneToJID(req.Phone)
	} else {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "phone or groupId required"})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_, err = client.RevokeMessage(context.Background(), jid, types.MessageID(req.MessageID))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Revoke failed: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service": "whatsmeow",
		"connected": connected,
		"endpoints": map[string][]string{
			"public": {"/health", "/qr", "/status"},
			"api":    {"/api/send", "/api/send-group", "/api/groups", "/api/group/{jid}", "/api/contacts", "/api/revoke"},
		},
		"auth": apiKey != "",
	})
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func isConnected() bool {
	qrMutex.Lock()
	defer qrMutex.Unlock()
	return connected
}

func parsePhoneToJID(phone string) (types.JID, error) {
	cleaned := ""
	for _, c := range phone {
		if c >= '0' && c <= '9' {
			cleaned += string(c)
		}
	}
	if len(cleaned) < 7 {
		return types.EmptyJID, fmt.Errorf("invalid phone number: %s", phone)
	}
	return types.NewJID(cleaned, types.DefaultUserServer), nil
}

