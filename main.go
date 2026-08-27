package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
)

var (
	client       *whatsmeow.Client
	qrMutex      sync.Mutex
	currentQR    string
	connected    bool
	reconnectMu  sync.Mutex
	isReconnecting bool
)

type SendRequest struct {
	Phone   string `json:"phone"`   // E.164 format, e.g. "1234567890"
	Message string `json:"message"` // Text message content
}

type SendResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type QRResponse struct {
	QR        string `json:"qr,omitempty"`
	Connected bool   `json:"connected"`
}

type StatusResponse struct {
	Connected bool   `json:"connected"`
	Phone     string `json:"phone,omitempty"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("Failed to create data dir %s: %v", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "whatsmeow.db")
	log.Printf("Using session database at: %s", dbPath)

	// WhatsApp session store backed by SQLite
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

	// Connect in background — will loop with auto-reconnect
	go connectLoop()

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/qr", qrHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/send", sendHandler)
	mux.HandleFunc("/", rootHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
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

// connectLoop keeps trying to connect — generates fresh QR codes after
// timeouts, and auto-reconnects when already paired.
func connectLoop() {
	for {
		if client.Store.ID != nil {
			// Already paired — just connect
			if !client.IsConnected() {
				log.Println("Session found, connecting to WhatsApp...")
				err := client.Connect()
				if err != nil {
					log.Printf("Failed to connect: %v — retrying in 5s", err)
					time.Sleep(5 * time.Second)
					continue
				}
			}
			// If already connected, wait for disconnect events
			time.Sleep(5 * time.Second)
			continue
		}

		// Not paired — generate QR codes.
		// GetQRChannel must be called BEFORE Connect.
		// We need to disconnect first if stale, then reconnect fresh.
		if client.IsConnected() {
			client.Disconnect()
		}

		log.Println("No session found. Generating QR code for pairing...")
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			// If GetQRChannel fails, it usually means we need to disconnect first
			log.Printf("Failed to get QR channel: %v — retrying in 3s", err)
			client.Disconnect()
			time.Sleep(3 * time.Second)
			continue
		}

		// Consume QR events in a goroutine
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

		// Wait for either QR timeout or successful pairing
		<-qrDone

		// Check if we got paired during that session
		if client.Store.ID != nil {
			log.Println("Paired! Switching to connected mode.")
			qrMutex.Lock()
			currentQR = ""
			qrMutex.Unlock()
			continue // Will enter the paired branch above
		}

		// QR timed out — disconnect, wait, and try again with fresh codes
		log.Println("QR timed out. Reconnecting to generate fresh codes...")
		client.Disconnect()
		qrMutex.Lock()
		currentQR = ""
		qrMutex.Unlock()
		time.Sleep(2 * time.Second)
	}
}

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
		sender := ""
		if !v.Info.Sender.IsEmpty() {
			sender = v.Info.Sender.String()
		}
		conversation := ""
		if conv := v.Message.GetConversation(); conv != "" {
			conversation = conv
		}
		log.Printf("Received message from %s: %s", sender, conversation)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
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

	resp := QRResponse{QR: qr, Connected: conn}
	if qr == "" && !conn {
		resp.QR = ""
	}
	json.NewEncoder(w).Encode(resp)
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

	qrMutex.Lock()
	conn := connected
	qrMutex.Unlock()

	if !conn {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(SendResponse{Error: "WhatsApp not connected. Pair first via /qr"})
		return
	}

	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "Invalid JSON: " + err.Error()})
		return
	}

	if req.Phone == "" || req.Message == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: "phone and message required"})
		return
	}

	// Parse JID from phone number
	jid, err := parsePhoneToJID(req.Phone)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SendResponse{Error: err.Error()})
		return
	}

	_, err = client.SendMessage(r.Context(), jid, &waE2E.Message{
		Conversation: &req.Message,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(SendResponse{Error: "Send failed: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(SendResponse{Success: true})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service":   "whatsmeow",
		"endpoints": []string{"/health", "/qr", "/status", "/send"},
		"connected": connected,
	})
}

func parsePhoneToJID(phone string) (types.JID, error) {
	// Strip non-digits
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
