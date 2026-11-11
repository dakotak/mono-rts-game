# All tools are pinned in tools/go.mod; nothing needs to be installed
# globally besides Go itself.
export GOTOOLCHAIN := local
TOOL := go tool -modfile=tools/go.mod

.PHONY: all generate build test race lint fmt bench scale sim run-server run-gateway dev vs-bot bot-arena bot-ladder bot-bench

all: lint test build

generate: ## Regenerate Go bindings from proto/
	$(TOOL) buf lint
	$(TOOL) buf generate

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

race:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal web examples)" || (gofmt -l cmd internal web examples; echo "run 'make fmt'"; exit 1)
	go vet ./...
	$(TOOL) staticcheck ./...
	$(TOOL) buf lint

fmt:
	gofmt -w cmd internal web examples

bench:
	go test -run '^$$' -bench BenchmarkStep -benchmem ./internal/game

scale: ## Scale scenarios (1k/5k/10k/25k units x 7 workloads) vs the 100 ms tick budget
	go run ./cmd/rts-bench

sim: ## Headless harness: 5000 units x 2000 ticks
	go run ./cmd/rts-sim -units 5000 -ticks 2000

run-server:
	go run ./cmd/rts-server -config configs/dev.json

run-gateway:
	go run ./cmd/rts-gateway -web web/static

dev: ## Server + gateway together; open http://127.0.0.1:8080 in two tabs
	./scripts/dev.sh

vs-bot: ## Play against the computer: server (duel) + bot host + gateway; BOT_LEVEL=7 adds one at once
	./scripts/vs-bot.sh

bot-arena: ## Bot-vs-bot evaluation: A vs B over map seeds (A=10 B=1 SEEDS=1-20)
	go run ./cmd/mono-arena -a $(or $(A),10) -b $(or $(B),1) -seeds $(or $(SEEDS),1-20)

bot-ladder: ## Every adjacent difficulty pair plus 10 vs 1 (SEEDS=1-20)
	go run ./cmd/mono-arena -ladder -seeds $(or $(SEEDS),1-20)

bot-bench: ## Computer player decision benchmarks at 100/1k/5k/10k units
	go test -run '^$$' -bench BenchmarkDecide -benchmem ./internal/bot/ai
