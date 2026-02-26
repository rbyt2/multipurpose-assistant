import json
import logging
import os
import queue
import sys
import time
from dataclasses import dataclass
from typing import Optional

import numpy as np
import pvporcupine
import requests
import sounddevice as sd
import vosk
import websocket


CONFIG_PATH = os.path.join(os.path.dirname(__file__), "config.json")
LOG_DIR = "/var/log/ai-assistant"
LOG_PATH = os.path.join(LOG_DIR, "voice_handler.log")


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
            format="%(asctime)s [voice_handler] %(levelname)s: %(message)s",
        )
    except Exception:
        # Fallback to stderr if log dir not writable
        logging.basicConfig(
            stream=sys.stderr,
            level=logging.INFO,
            format="%(asctime)s [voice_handler] %(levelname)s: %(message)s",
        )


class StatusClient:
    def __init__(self, cfg: Config):
        self._cfg = cfg
        self._ws: Optional[websocket.WebSocketApp] = None
        self._thread = None

    def _url(self) -> str:
        return f"ws://localhost:{self._cfg.server_port}/ws"

    def _on_open(self, ws):
        logging.info("WebSocket connected for status updates")

    def _on_close(self, ws, close_status_code, close_msg):
        logging.info("WebSocket closed: %s %s", close_status_code, close_msg)

    def _on_error(self, ws, error):
        logging.warning("WebSocket error: %s", error)

    def connect(self):
        def run():
            while True:
                try:
                    self._ws = websocket.WebSocketApp(
                        self._url(),
                        on_open=self._on_open,
                        on_close=self._on_close,
                        on_error=self._on_error,
                    )
                    self._ws.run_forever()
                except Exception as e:
                    logging.warning("WebSocket run_forever error: %s", e)
                time.sleep(2)

        import threading

        self._thread = threading.Thread(target=run, daemon=True)
        self._thread.start()

    def send_status(self, state: str) -> None:
        msg = json.dumps({"type": "status", "state": state})
        try:
            if self._ws and self._ws.sock and self._ws.sock.connected:
                self._ws.send(msg)
        except Exception as e:
            logging.debug("Failed to send status over WebSocket: %s", e)


class SpeechEngine:
    def __init__(self, cfg: Config):
        import pyttsx3

        self._engine = pyttsx3.init()
        self._engine.setProperty("rate", cfg.tts_rate)
        if cfg.tts_voice and cfg.tts_voice != "default":
            for voice in self._engine.getProperty("voices"):
                if cfg.tts_voice.lower() in voice.name.lower():
                    self._engine.setProperty("voice", voice.id)
                    break

    def speak(self, text: str) -> None:
        if not text:
            return
        self._engine.say(text)
        self._engine.runAndWait()


class VoiceHandler:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.status = StatusClient(cfg)
        self.speech = SpeechEngine(cfg)

        if not os.path.isdir(cfg.vosk_model_path):
            logging.error("Vosk model not found at %s", cfg.vosk_model_path)
            raise SystemExit(1)

        self.vosk_model = vosk.Model(cfg.vosk_model_path)
        self.recognizer = vosk.KaldiRecognizer(self.vosk_model, 16000)

        self._audio_queue: "queue.Queue[np.ndarray]" = queue.Queue()
        self._stop = False

    def _create_porcupine(self) -> pvporcupine.Porcupine:
        kwargs = {
            "access_key": self.cfg.picovoice_api_key,
            "keywords": [self.cfg.wake_word],
        }
        return pvporcupine.create(**kwargs)

    def listen_for_command(self, timeout: float = 5.0) -> Optional[str]:
        duration = float(timeout)
        logging.info("Capturing command audio for %.1fs", duration)

        try:
            audio = sd.rec(
                int(duration * 16000),
                samplerate=16000,
                channels=1,
                dtype="int16",
                device=self.cfg.microphone_index,
            )
            sd.wait()
        except Exception as e:
            logging.error("Failed to capture command audio: %s", e)
            return None

        data = audio.tobytes()
        self.recognizer.Reset()
        if self.recognizer.AcceptWaveform(data):
            result = json.loads(self.recognizer.Result())
        else:
            result = json.loads(self.recognizer.FinalResult())

        text = (result or {}).get("text", "").strip()
        logging.info("Recognized command: %s", text)
        return text or None

    def _post_query(self, text: str, mode: str = "voice") -> Optional[str]:
        payload = {"text": text, "mode": mode}
        url = f"http://localhost:{self.cfg.server_port}/query"

        backoff = 0.5
        for attempt in range(3):
            try:
                resp = requests.post(url, json=payload, timeout=10)
                if resp.ok:
                    data = resp.json()
                    return data.get("response", "")
                logging.warning("Backend /query error: %s %s", resp.status_code, resp.text)
            except Exception as e:
                logging.warning("Backend /query request failed (attempt %d): %s", attempt + 1, e)
            time.sleep(backoff)
            backoff *= 2

        return None

    def run(self):
        self.status.connect()
        porcupine = None

        try:
            porcupine = self._create_porcupine()
        except Exception as e:
            logging.error("Failed to initialise Porcupine: %s", e)
            raise SystemExit(1)

        logging.info("Porcupine initialised. Listening for wake word '%s'", self.cfg.wake_word)

        try:
            with sd.InputStream(
                samplerate=porcupine.sample_rate,
                blocksize=porcupine.frame_length,
                dtype="int16",
                channels=1,
                device=self.cfg.microphone_index,
            ) as stream:
                while not self._stop:
                    try:
                        pcm = stream.read(porcupine.frame_length)[0]
                    except OSError as e:
                        logging.error("Microphone input error: %s", e)
                        raise SystemExit(1)

                    pcm = np.squeeze(pcm).astype(np.int16)
                    keyword_index = porcupine.process(pcm)

                    if keyword_index >= 0:
                        logging.info("Wake word detected")
                        self.status.send_status("listening")

                        command = self.listen_for_command(timeout=5.0)
                        if not command:
                            logging.info("No command detected after wake word")
                            self.status.send_status("idle")
                            continue

                        self.status.send_status("thinking")
                        reply = self._post_query(command, mode="voice")
                        if reply is None:
                            logging.error("Failed to get reply from backend")
                            self.speech.speak("Sorry, I had trouble processing that.")
                            self.status.send_status("idle")
                            continue

                        self.status.send_status("speaking")
                        self.speech.speak(reply)
                        self.status.send_status("idle")
        finally:
            if porcupine is not None:
                porcupine.delete()


def main():
    setup_logging()
    logging.info("Starting voice handler")

    try:
        cfg = load_config(CONFIG_PATH)
    except Exception as e:
        logging.error("Failed to load config: %s", e)
        sys.exit(1)

    try:
        handler = VoiceHandler(cfg)
        handler.run()
    except KeyboardInterrupt:
        logging.info("Voice handler interrupted by user")
    except SystemExit:
        raise
    except Exception as e:
        logging.exception("Fatal error in voice handler: %s", e)


if __name__ == "__main__":
    main()

