package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/gorilla/websocket"
	"google.golang.org/api/option"
)

type Config struct {
	GeminiAPIKey    string `json:"gemini_api_key"`
	PicovoiceAPIKey string `json:"picovoice_api_key"`
	WakeWord        string `json:"wake_word"`
	TTSRate         int    `json:"tts_rate"`
	TTSVoice        string `json:"tts_voice"`
	ServerPort      int    `json:"server_port"`
	WebsocketPort   int    `json:"websocket_port"`
	VoskModelPath   string `json:"vosk_model_path"`
	CameraIndex     int    `json:"camera_index"`
	MicIndex        int    `json:"microphone_index"`
}

type QueryRequest struct {
	Text  string `json:"text"`
	Image string `json:"image,omitempty"` // base64 encoded (no data URL prefix)
	Mode  string `json:"mode"`            // "voice" or "text"
}

type QueryResponse struct {
	Success    bool                   `json:"success"`
	Response   string                 `json:"response"`
	NeedsChart bool                   `json:"needsChart"`
	ChartData  map[string]interface{} `json:"chartData,omitempty"`
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

type wsHub struct {
	mu       sync.RWMutex
	clients  map[*wsClient]struct{}
	register chan *wsClient
	unreg    chan *wsClient
	bcast    chan []byte
}

type conversationTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

type server struct {
	cfg          *Config
	geminiClient *genai.Client
	model        *genai.GenerativeModel
	hub          *wsHub

	convMu sync.Mutex
	conv   []conversationTurn
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// CORS is handled separately; allow all WS origins here.
		return true
	},
}

func newHub() *wsHub {
	return &wsHub{
		clients:  make(map[*wsClient]struct{}),
		register: make(chan *wsClient),
		unreg:    make(chan *wsClient),
		bcast:    make(chan []byte, 32),
	}
}

func (h *wsHub) run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c] = struct{}{}
			h.mu.Unlock()
		case c := <-h.unreg:
			h.mu.Lock()
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}
			h.mu.Unlock()
		case msg := <-h.bcast:
			h.mu.RLock()
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// Drop slow client
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (c *wsClient) readPump(h *wsHub) {
	defer func() {
		h.unreg <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(1024 * 8)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		 c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		 return nil
	})
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[ws] read error: %v", err)
			}
			break
		}
		// Echo/broadcast any message from any client (voice handler or others).
		h.bcast <- message
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(50 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func wsHandler(s *server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[ws] upgrade error: %v", err)
			return
		}
		client := &wsClient{
			conn: conn,
			send: make(chan []byte, 16),
		}
		s.hub.register <- client

		go client.writePump()
		go client.readPump(s.hub)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] %v\n%s", rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) appendConversation(user, assistant string) {
	s.convMu.Lock()
	defer s.convMu.Unlock()
	turn := conversationTurn{User: user, Assistant: assistant}
	s.conv = append(s.conv, turn)
	if len(s.conv) > 5 {
		s.conv = s.conv[len(s.conv)-5:]
	}
}

func (s *server) clearConversation() {
	s.convMu.Lock()
	defer s.convMu.Unlock()
	s.conv = nil
}

func (s *server) conversationContext() string {
	s.convMu.Lock()
	defer s.convMu.Unlock()
	if len(s.conv) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range s.conv {
		b.WriteString("User: ")
		b.WriteString(t.User)
		b.WriteString("\nAssistant: ")
		b.WriteString(t.Assistant)
		b.WriteString("\n")
	}
	return b.String()
}

func (s *server) broadcastStatus(state string) {
	msg := map[string]string{
		"type":  "status",
		"state": state,
	}
	data, _ := json.Marshal(msg)
	s.hub.bcast <- data
}

func (s *server) broadcastResponse(text string, needsChart bool, chartData map[string]interface{}) {
	msg := map[string]interface{}{
		"type":       "response",
		"text":       text,
		"needsChart": needsChart,
		"chartData":  chartData,
	}
	data, _ := json.Marshal(msg)
	s.hub.bcast <- data
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join("templates", "index.html"))
}

func (s *server) handleVisionUI(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join("templates", "visualizer.html"))
}

func decodeBase64Image(b64 string) ([]byte, string, error) {
	if b64 == "" {
		return nil, "", errors.New("empty image data")
	}
	// Strip data URL prefix if present.
	if idx := strings.Index(b64, ","); idx != -1 {
		b64 = b64[idx+1:]
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, "", err
	}
	// Default mime type; frontend can adjust if needed.
	return data, "image/jpeg", nil
}

func needsChartForText(t string) bool {
	l := strings.ToLower(t)
	keywords := []string{"chart", "graph", "plot", "diagram", "visualize", "show me", "compare"}
	for _, kw := range keywords {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

func (s *server) buildGeminiInput(req *QueryRequest, forceChart bool) ([]*genai.Content, error) {
	var parts []genai.Part

	contextText := s.conversationContext()
	if contextText != "" {
		parts = append(parts, genai.Text("Conversation so far:\n"+contextText))
	}

	if req.Text != "" {
		parts = append(parts, genai.Text("User query: "+req.Text))
	}

	if req.Image != "" {
		imgBytes, mime, err := decodeBase64Image(req.Image)
		if err != nil {
			return nil, fmt.Errorf("invalid image data: %w", err)
		}
		parts = append(parts, genai.Blob{
			MIMEType: mime,
			Data:     imgBytes,
		})
	}

	chartInstruction := "Respond in natural language only."
	if forceChart || needsChartForText(req.Text) {
		chartInstruction = "If this query is suitable for a chart or visualisation, respond ONLY with a compact JSON object of the form {\"response\": string, \"needsChart\": true|false, \"chartData\": { ... }}. " +
			"When needsChart is true, chartData MUST contain labels and datasets suitable for Chart.js, for example {\"type\": \"bar\", \"labels\": [\"A\",\"B\"], \"datasets\": [{\"label\":\"Series\",\"data\":[1,2]}]}. " +
			"Do not include any explanations outside the JSON."
	} else {
		chartInstruction = "Respond with a concise helpful answer in plain text. Do not include JSON unless explicitly asked."
	}

	parts = append(parts, genai.Text(chartInstruction))

	return []*genai.Content{
		{
			Parts: parts,
		},
	}, nil
}

func (s *server) callGemini(ctx context.Context, req *QueryRequest, forceChart bool) (*QueryResponse, error) {
	if s.model == nil {
		return nil, errors.New("gemini model not configured")
	}

	input, err := s.buildGeminiInput(req, forceChart)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	resp, err := s.model.GenerateContent(ctx, input...)
	if err != nil {
		return nil, err
	}
	latency := time.Since(start)
	log.Printf("[gemini] request completed in %s", latency)

	if resp == nil || len(resp.Candidates) == 0 {
		return nil, errors.New("empty response from model")
	}

	var sb strings.Builder
	for _, cand := range resp.Candidates {
		for _, p := range cand.Content.Parts {
			if t, ok := p.(genai.Text); ok {
				sb.WriteString(string(t))
			}
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return nil, errors.New("no text in model response")
	}

	// Attempt to parse JSON for chart responses.
	var parsed QueryResponse
	if (forceChart || needsChartForText(req.Text)) && strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			if parsed.Response == "" && parsed.Success {
				parsed.Response = "I generated chart data based on your request."
			}
			if !parsed.Success {
				parsed.Success = true
			}
			return &parsed, nil
		}
		// If JSON parsing fails, fall back to plain text handling below.
		log.Printf("[gemini] failed to parse JSON chart response, falling back to text: %v", err)
	}

	return &QueryResponse{
		Success:    true,
		Response:   text,
		NeedsChart: false,
	}, nil
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[/query] decode error: %v", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" && req.Image == "" {
		http.Error(w, "text or image required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	s.broadcastStatus("thinking")
	gResp, err := s.callGemini(ctx, &req, false)
	if err != nil {
		log.Printf("[/query] gemini error: %v", err)
		errResp := QueryResponse{
			Success:  false,
			Response: "Sorry, I had trouble processing that request.",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(errResp)
		return
	}

	s.appendConversation(req.Text, gResp.Response)
	s.broadcastResponse(gResp.Response, gResp.NeedsChart, gResp.ChartData)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gResp)
}

func (s *server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[/analyze] decode error: %v", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Image == "" {
		http.Error(w, "image required", http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		req.Text = "Describe the image in detail."
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	s.broadcastStatus("thinking")
	gResp, err := s.callGemini(ctx, &req, false)
	if err != nil {
		log.Printf("[/analyze] gemini error: %v", err)
		errResp := QueryResponse{
			Success:  false,
			Response: "Sorry, I couldn't analyze the image.",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(errResp)
		return
	}

	s.appendConversation(req.Text, gResp.Response)
	s.broadcastResponse(gResp.Response, gResp.NeedsChart, gResp.ChartData)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gResp)
}

func (s *server) handleVisionQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[/vision-query] decode error: %v", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Image == "" && strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text or image required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	s.broadcastStatus("thinking")
	gResp, err := s.callGemini(ctx, &req, false)
	if err != nil {
		log.Printf("[/vision-query] gemini error: %v", err)
		errResp := QueryResponse{
			Success:  false,
			Response: "Sorry, I couldn't process that vision request.",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(errResp)
		return
	}

	s.appendConversation(req.Text, gResp.Response)
	s.broadcastResponse(gResp.Response, gResp.NeedsChart, gResp.ChartData)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gResp)
}

func (s *server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.clearConversation()
	w.WriteHeader(http.StatusNoContent)
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 5000
	}
	if cfg.WebsocketPort == 0 {
		cfg.WebsocketPort = 8080
	}
	if cfg.TTSRate == 0 {
		cfg.TTSRate = 150
	}
	return &cfg, nil
}

func setupLogging() {
	logDir := "/var/log/ai-assistant"
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Printf("could not create log dir %s: %v", logDir, err)
		return
	}
	logFile := filepath.Join(logDir, "backend.log")
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("could not open log file %s: %v", logFile, err)
		return
	}
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func initGeminiClient(ctx context.Context, apiKey string) (*genai.Client, *genai.GenerativeModel, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}
	if apiKey == "" {
		return nil, nil, errors.New("missing Gemini API key")
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, nil, err
	}
	model := client.GenerativeModel("gemini-1.5-flash")
	model.SetTemperature(0.7)
	return client, model, nil
}

func main() {
	// More aggressive GC on constrained hardware.
	debug.SetGCPercent(50)

	// Log to file and fall back to stderr if needed.
	setupLogging()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Printf("failed to load config.json: %v", err)
		// Still start, but Gemini calls will fail until config is fixed.
	}

	apiKey := ""
	if cfg != nil {
		apiKey = cfg.GeminiAPIKey
	}

	gClient, gModel, err := initGeminiClient(ctx, apiKey)
	if err != nil {
		log.Printf("Gemini init error: %v (service will run but /query will fail until configured)", err)
	}

	hub := newHub()
	go hub.run()

	srv := &server{
		cfg:          cfg,
		geminiClient: gClient,
		model:        gModel,
		hub:          hub,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/visualizer", srv.handleVisionUI)
	mux.HandleFunc("/query", srv.handleQuery)
	mux.HandleFunc("/analyze", srv.handleAnalyze)
	mux.HandleFunc("/vision-query", srv.handleVisionQuery)
	mux.HandleFunc("/clear-history", srv.handleClearHistory)
	mux.HandleFunc("/ws", wsHandler(srv))

	// Static assets
	fs := http.FileServer(http.Dir("static"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))

	handler := recoverMiddleware(corsMiddleware(mux))

	port := 5000
	if cfg != nil && cfg.ServerPort != 0 {
		port = cfg.ServerPort
	}

	addr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("AI Assistant backend listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}

	if gClient != nil {
		_ = gClient.Close()
	}
	log.Println("server stopped")
}

