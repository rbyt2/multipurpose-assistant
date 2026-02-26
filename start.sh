#!/usr/bin/env bash

set -euo pipefail

echo "Starting AI Assistant..."

cd "$(dirname "$0")"

if [ ! -f config.json ]; then
  echo "ERROR: config.json not found"
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required but not installed. Run ./install.sh first."
  exit 1
fi

export GOOGLE_API_KEY
GOOGLE_API_KEY="$(jq -r '.gemini_api_key' config.json)"

if [ -z "$GOOGLE_API_KEY" ] || [ "$GOOGLE_API_KEY" = "null" ]; then
  echo "WARNING: gemini_api_key in config.json is empty. Gemini calls will fail."
fi

python3 orchestrator.py

