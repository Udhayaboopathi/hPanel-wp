GO ?= go
BIN := bin

.PHONY: all build test vet lint run-panel run-agent tidy clean bench-size
all: build

build:
	mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN)/panel ./cmd/panel
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN)/agent ./cmd/agent

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run-panel: build
	PANEL_ADDR=:8080 PANEL_DATA_DIR=./data SITES_DIR=./data/websites BACKUPS_DIR=./data/backups \
	AGENT_SOCKET=./data/agent.sock AGENT_TOKEN_FILE=./data/agent.token ./$(BIN)/panel

clean:
	rm -rf $(BIN)

bench-size: build
	ls -lh $(BIN)/panel $(BIN)/agent
