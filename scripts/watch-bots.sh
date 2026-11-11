#!/usr/bin/env bash
# Watch computer players fight: starts rts-server (3-player preset), the
# gateway and three mono-bot players, then opens the admin console signed
# in, to watch the match live. Ctrl-C stops everything.
#
#   BOTS="9:aggressive 6:economic 3:defensive" make watch-bots
#
# Each entry is difficulty[:personality], one per slot (Red, Blue, Green).
set -euo pipefail
cd "$(dirname "$0")/.."

CONFIG="${CONFIG:-configs/presets/trio.json}"
BOTS="${BOTS:-8:aggressive 5:balanced 3:economic}"
BOT_LOG="${BOT_LOG:-warn}"            # mono-bot log level: debug, info, warn, error
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:50051}"
HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:8080}"
ADMIN_ADDR="${ADMIN_ADDR:-127.0.0.1:8081}"
OPEN="${OPEN:-1}"                      # 0: do not open a browser

# Join tokens and the admin token come from the configuration.
read -r -a TOKENS <<<"$(python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); print(" ".join(p["join_token"] for p in sorted(c["players"], key=lambda p: p["slot"])))' "$CONFIG")"
ADMIN_TOKEN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["admin_token"])' "$CONFIG")"
read -r -a SPECS <<<"$BOTS"
if [ "${#SPECS[@]}" -gt "${#TOKENS[@]}" ]; then
  echo "BOTS lists ${#SPECS[@]} players but $CONFIG has ${#TOKENS[@]} slots" >&2
  exit 2
fi

mkdir -p bin
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server ./cmd/rts-gateway ./cmd/mono-bot

trap 'kill 0' EXIT INT TERM
bin/rts-server -config "$CONFIG" -addr "$GRPC_ADDR" &
bin/rts-gateway -server "$GRPC_ADDR" -addr "$HTTP_ADDR" -admin-addr "$ADMIN_ADDR" -web web/static &

# Wait for the gateway before starting bots and the browser.
for _ in $(seq 1 50); do
  curl -fs -o /dev/null "http://$ADMIN_ADDR/" && break
  sleep 0.2
done

for i in "${!SPECS[@]}"; do
  spec="${SPECS[$i]}"
  level="${spec%%:*}"
  personality="balanced"
  [[ "$spec" == *:* ]] && personality="${spec#*:}"
  bin/mono-bot -server "$GRPC_ADDR" -token "${TOKENS[$i]}" -difficulty "$level" -personality "$personality" \
    -player-name "slot$((i + 1))-L$level-$personality" -log-level "$BOT_LOG" &
  echo "Slot $((i + 1)): computer level $level, $personality"
done

URL="http://$ADMIN_ADDR/#token=$ADMIN_TOKEN"
echo "Admin console: http://$ADMIN_ADDR (token $ADMIN_TOKEN). Use \"View as\" to see a bot's fog of war."
if [ "$OPEN" = "1" ]; then
  if command -v open >/dev/null; then open "$URL"
  elif command -v xdg-open >/dev/null; then xdg-open "$URL" >/dev/null 2>&1
  else echo "Open $URL in a browser."; fi
fi
wait
