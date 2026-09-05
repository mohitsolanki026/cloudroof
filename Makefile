.PHONY: all web build run dev test e2e release clean docker

BIN     := bin/bosun
VERSION ?= 1.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)

all: web build

# Frontend bundle -> web/dist (embedded into the binary by `build`).
web:
	cd web && npm ci --no-audit --no-fund && npm run build

# Single static binary. CGO is off because the SQLite driver is pure Go.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/bosun

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

# Cross-compiled release tarballs + checksums in dist/. The frontend is built
# once and embedded into every binary; pure-Go (CGO off) makes this trivial.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
release: web
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o dist/bosun ./cmd/bosun; \
		tar -C dist -czf dist/bosun_$(VERSION)_$${os}_$${arch}.tar.gz bosun; \
		rm -f dist/bosun; \
	done
	@cd dist && sha256sum *.tar.gz > checksums.txt
	@echo "release $(VERSION):" && ls -1 dist

clean:
	rm -rf bin dist web/dist/* data
	touch web/dist/.gitkeep

docker:
	docker build -t bosun:$(VERSION) .
