.PHONY: all web build run dev test clean docker

BIN     := bin/bosun
VERSION ?= 0.1.0-dev

all: web build

# Frontend bundle -> web/dist (embedded into the binary by `build`).
web:
	cd web && npm ci --no-audit --no-fund && npm run build

# Single static binary. CGO is off because the SQLite driver is pure Go.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/bosun

run: build
	$(BIN) -data ./data

# Backend with the frontend served from web/dist on disk; pair with `npm run dev`
# in web/ for hot reload (vite proxies /api to :7070).
dev:
	BOSUN_LOG=debug go run ./cmd/bosun -dev -data ./data

test:
	go vet ./...
	go test ./...

# Full end-to-end against a throwaway local sshd. See scripts/e2e.sh.
e2e:
	bash scripts/e2e.sh

clean:
	rm -rf bin web/dist/* data
	touch web/dist/.gitkeep

docker:
	docker build -t bosun:$(VERSION) .
