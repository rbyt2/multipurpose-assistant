#!/usr/bin/env bash
set -euo pipefail

echo "[install] Updating system packages..."
sudo apt update
sudo apt upgrade -y

echo "[install] Installing system dependencies..."
sudo apt install -y \
  git python3-pip python3-pyaudio portaudio19-dev \
  libatlas-base-dev pulseaudio alsa-utils chromium-browser \
  espeak libespeak-dev jq unzip wget

echo "[install] Installing Python packages..."
pip3 install --upgrade pip
pip3 install pvporcupine vosk sounddevice pyttsx3 websocket-client requests

echo "[install] Installing Go 1.21.x..."
cd "$(dirname "$0")"
GO_ARCHIVE="go1.21.6.linux-arm64.tar.gz"
if [ ! -f "$GO_ARCHIVE" ]; then
  wget "https://go.dev/dl/${GO_ARCHIVE}"
fi
sudo tar -C /usr/local -xzf "$GO_ARCHIVE"
if ! grep -q "/usr/local/go/bin" <<<"$PATH"; then
  echo 'export PATH=$PATH:/usr/local/go/bin' >> "${HOME}/.bashrc"
fi
export PATH=$PATH:/usr/local/go/bin

echo "[install] Downloading Vosk model (small English)..."
VOSK_ZIP="vosk-model-small-en-us-0.15.zip"
if [ ! -f "$VOSK_ZIP" ]; then
  wget "https://alphacephei.com/vosk/models/${VOSK_ZIP}"
fi
unzip -o "$VOSK_ZIP"
mkdir -p "${HOME}/vosk-model"
mv -f vosk-model-small-en-us-0.15/* "${HOME}/vosk-model/" || true

echo "[install] Enabling Raspberry Pi camera..."
if command -v raspi-config >/dev/null 2>&1; then
  sudo raspi-config nonint do_camera 0 || true
fi

echo "[install] Creating log directory..."
sudo mkdir -p /var/log/ai-assistant
sudo chown "${USER}:${USER}" /var/log/ai-assistant || true

echo "[install] Done. Please log out/in to pick up PATH changes if needed."

