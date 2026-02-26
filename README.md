## Raspberry Pi AI Assistant

This project is a multi‑modal AI assistant for Raspberry Pi 3 B+, with:

- **Go backend** exposing REST and WebSocket APIs
- **Python voice handler** for wake‑word, ASR (Vosk), and TTS (pyttsx3)
- **HTML/CSS/JS frontend** with animated visualizer and Chart.js charts

### Quick start (development on Pi)

1. **Install dependencies**

```bash
chmod +x install.sh
./install.sh
```

2. **Configure API keys**

Edit `config.json` and set:

- `gemini_api_key`
- `picovoice_api_key`

3. **Build and run backend**

```bash
cd ai-assistant
GOOS=linux GOARCH=arm go build -o ai-backend
./ai-backend
```

4. **Run orchestrator (starts everything)**

```bash
chmod +x start.sh
./start.sh
```

The UI is available at `http://<pi-ip>:5000/` (main) and `/visualizer` (embedded visualizer).

