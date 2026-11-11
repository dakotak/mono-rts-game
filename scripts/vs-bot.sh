#!/usr/bin/env bash
# Play against the computer: starts rts-server (duel preset), a bot host for
# the second slot and the gateway connected to it. Join as duel-red in the
# browser and add the computer opponent from the "Computer opponents" panel
# (or set BOT_LEVEL to have one join right away). Ctrl-C stops everything.
set -euo pipefail
cd "$(dirname "$0")/.."

CONFIG="${CONFIG:-configs/presets/duel.json}"
BOT_SLOTS="${BOT_SLOTS:-2}"
BOT_LEVEL="${BOT_LEVEL:-}"
BOT_PERSONALITY="${BOT_PERSONALITY:-balanced}"
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:50051}"
HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:8080}"
ADMIN_ADDR="${ADMIN_ADDR:-127.0.0.1:8081}"
BOT_ADDR="${BOT_ADDR:-127.0.0.1:8090}"

mkdir -p bin
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server ./cmd/rts-gateway ./cmd/mono-bothost

trap 'kill 0' EXIT INT TERM
bin/rts-server -config "$CONFIG" -addr "$GRPC_ADDR" &
bin/mono-bothost -server "$GRPC_ADDR" -addr "$BOT_ADDR" -config "$CONFIG" -slots "$BOT_SLOTS" &
bin/rts-gateway -server "$GRPC_ADDR" -addr "$HTTP_ADDR" -admin-addr "$ADMIN_ADDR" -bothost "http://$BOT_ADDR" -web web/static &
if [ -n "$BOT_LEVEL" ]; then
  sleep 1
  for slot in ${BOT_SLOTS//,/ }; do
    curl -fsS -X POST "http://$BOT_ADDR/api/bots" -H 'Content-Type: application/json' \
      -d "{\"slot\":$slot,\"difficulty\":$BOT_LEVEL,\"personality\":\"$BOT_PERSONALITY\"}" >/dev/null
    echo "Computer player (level $BOT_LEVEL, $BOT_PERSONALITY) joined slot $slot."
  done
fi
echo "Open http://$HTTP_ADDR and join with the first player's token from $CONFIG (duel-red for the duel preset)."
wait
