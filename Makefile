BINARY  := gatekeeper
PKG     := ./cmd/gatekeeper
BIN_DIR := bin
CONFIG  ?= examples/gatekeeper.json
IMAGE   ?= gatekeeper:local

.PHONY: all build test lint fmt vet run run-echo docker docker-up docker-down clean

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY) $(PKG)

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$unformatted"; exit 1; \
	fi

lint: vet fmt

run: build
	$(BIN_DIR)/$(BINARY) -config $(CONFIG)

# Starts two echo upstreams matching examples/gatekeeper.json. Ctrl-C stops both.
run-echo:
	go run ./examples/echo -addr :8081 -name echo1 & \
	go run ./examples/echo -addr :8082 -name echo2 & \
	wait

docker:
	docker build -t $(IMAGE) .

docker-up:
	docker compose up --build

docker-down:
	docker compose down

clean:
	rm -rf $(BIN_DIR)
