import json
import logging
import os
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from typing import Optional

import requests


BASE_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG_PATH = os.path.join(BASE_DIR, "config.json")
LOG_DIR = "/var/log/ai-assistant"
LOG_PATH = os.path.join(LOG_DIR, "orchestrator.log")


@dataclass
class Config:
    gemini_api_key: str
    picovoice_api_key: str
    wake_word: str
    tts_rate: int
    tts_voice: str
    server_port: int
    websocket_port: int
    vosk_model_path: str
    camera_index: int
    microphone_index: int


def load_config(path: str) -> Config:
    with open(path, "r", encoding="utf-8") as f:
        raw = json.load(f)
    return Config(
        gemini_api_key=raw.get("gemini_api_key", ""),
        picovoice_api_key=raw.get("picovoice_api_key", ""),
        wake_word=raw.get("wake_word", "computer"),
        tts_rate=int(raw.get("tts_rate", 150)),
        tts_voice=raw.get("tts_voice", "default"),
        server_port=int(raw.get("server_port", 5000)),
        websocket_port=int(raw.get("websocket_port", 8080)),
        vosk_model_path=raw.get("vosk_model_path", "/home/pi/vosk-model"),
        camera_index=int(raw.get("camera_index", 0)),
        microphone_index=int(raw.get("microphone_index", 0)),
    )


def setup_logging() -> None:
    try:
        os.makedirs(LOG_DIR, exist_ok=True)
        logging.basicConfig(
            filename=LOG_PATH,
            level=logging.INFO,
            format="%(asctime)s [orchestrator] %(levelname)s: %(message)s",
        )
    except Exception:
        logging.basicConfig(
            stream=sys.stderr,
            level=logging.INFO,
            format="%(asctime)s [orchestrator] %(levelname)s: %(message)s",
        )


def start_backend(cfg: Config) -> subprocess.Popen:
    binary_path = os.path.join(BASE_DIR, "ai-backend")
    if os.path.isfile(binary_path) and os.access(binary_path, os.X_OK):
        cmd = [binary_path]
    else:
        # Fallback to go run for development
        cmd = ["go", "run", "main.go"]
    logging.info("Starting backend: %s", " ".join(cmd))
    return subprocess.Popen(cmd, cwd=BASE_DIR)


def wait_for_backend(cfg: Config, timeout: float = 30.0) -> bool:
    url = f"http://localhost:{cfg.server_port}/"
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            resp = requests.get(url, timeout=2)
            if resp.status_code == 200:
                logging.info("Backend is healthy")
                return True
        except Exception:
            pass
        time.sleep(1)
    logging.error("Backend health check failed after %.1fs", timeout)
    return False


def start_chromium(cfg: Config) -> Optional[subprocess.Popen]:
    url = f"http://localhost:{cfg.server_port}/visualizer"
    cmd = [
        "chromium-browser",
        "--kiosk",
        "--start-fullscreen",
        "--window-size=800,480",
        url,
    ]
    try:
        logging.info("Starting Chromium kiosk: %s", " ".join(cmd))
        return subprocess.Popen(cmd)
    except FileNotFoundError:
        logging.warning("chromium-browser not found; skipping visualizer launch")
        return None
    except Exception as e:
        logging.error("Failed to start Chromium: %s", e)
        return None


def start_voice_handler(cfg: Config) -> subprocess.Popen:
    cmd = [sys.executable, os.path.join(BASE_DIR, "voice_handler.py")]
    logging.info("Starting voice handler: %s", " ".join(cmd))
    return subprocess.Popen(cmd, cwd=BASE_DIR)


def terminate_process(proc: Optional[subprocess.Popen], name: str, timeout: float = 5.0):
    if proc is None:
        return
    if proc.poll() is not None:
        return
    logging.info("Terminating %s (pid=%s)", name, proc.pid)
    try:
        proc.terminate()
        deadline = time.time() + timeout
        while time.time() < deadline and proc.poll() is None:
            time.sleep(0.2)
        if proc.poll() is None:
            logging.info("Killing %s (pid=%s)", name, proc.pid)
            proc.kill()
    except Exception as e:
        logging.warning("Error terminating %s: %s", name, e)


def main():
    setup_logging()
    logging.info("Starting orchestrator")

    try:
        cfg = load_config(CONFIG_PATH)
    except Exception as e:
        logging.error("Failed to load config: %s", e)
        sys.exit(1)

    backend_proc = start_backend(cfg)
    if not wait_for_backend(cfg):
        terminate_process(backend_proc, "backend")
        sys.exit(1)

    chromium_proc = start_chromium(cfg)
    voice_proc = start_voice_handler(cfg)

    shutting_down = False

    def handle_signal(signum, frame):
        nonlocal shutting_down
        if shutting_down:
            return
        shutting_down = True
        logging.info("Received signal %s, shutting down", signum)
        terminate_process(voice_proc, "voice_handler")
        terminate_process(chromium_proc, "chromium")
        terminate_process(backend_proc, "backend")
        sys.exit(0)

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    try:
        # Simple supervision loop – restart components if they crash.
        while True:
            if backend_proc.poll() is not None:
                logging.error("Backend exited with code %s; restarting", backend_proc.returncode)
                backend_proc = start_backend(cfg)
                if not wait_for_backend(cfg):
                    logging.error("Backend failed to restart; exiting orchestrator")
                    break

            if voice_proc.poll() is not None:
                logging.warning("Voice handler exited with code %s; restarting", voice_proc.returncode)
                voice_proc = start_voice_handler(cfg)

            if chromium_proc and chromium_proc.poll() is not None:
                logging.warning("Chromium exited with code %s; restarting", chromium_proc.returncode)
                chromium_proc = start_chromium(cfg)

            time.sleep(2)
    except KeyboardInterrupt:
        handle_signal(signal.SIGINT, None)


if __name__ == "__main__":
    main()

