#!/usr/bin/env bash
# Starts rts-server and rts-gateway together; Ctrl-C stops both.
set -euo pipefail
cd "$(dirname "$0")/.."

CONFIG="${CONFIG:-configs/dev.json}"
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:50051}"
HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:8080}"

mkdir -p bin
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server ./cmd/rts-gateway

trap 'kill 0' EXIT INT TERM
bin/rts-server -config "$CONFIG" -addr "$GRPC_ADDR" &
bin/rts-gateway -server "$GRPC_ADDR" -addr "$HTTP_ADDR" -web web/static &
echo "Open http://$HTTP_ADDR in two browser tabs; join as 'red' in one and 'blue' in the other."
wait
