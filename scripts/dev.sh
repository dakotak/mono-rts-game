#!/usr/bin/env bash
# Starts rts-server and rts-gateway together; Ctrl-C stops both.
set -euo pipefail
cd "$(dirname "$0")/.."

CONFIG="${CONFIG:-configs/dev.json}"
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:50051}"
HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:8080}"
ADMIN_ADDR="${ADMIN_ADDR:-127.0.0.1:8081}"

mkdir -p bin
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server ./cmd/rts-gateway

trap 'kill 0' EXIT INT TERM
bin/rts-server -config "$CONFIG" -addr "$GRPC_ADDR" &
bin/rts-gateway -server "$GRPC_ADDR" -addr "$HTTP_ADDR" -admin-addr "$ADMIN_ADDR" -web web/static &
echo "Players: open http://$HTTP_ADDR in two browser tabs; join as 'red' in one and 'blue' in the other."
echo "Admin:   open http://$ADMIN_ADDR and sign in with the admin_token from $CONFIG."
wait
