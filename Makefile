.PHONY: all web build slim run dev test e2e release clean docker

BIN     := bin/cloudroof
VERSION ?= 1.5.0
LDFLAGS := -s -w -X main.version=$(VERSION)

all: web build

# Frontend bundle -> web/dist (embedded into the binary by `build`).
web:
	cd web && npm ci --no-audit --no-fund && npm run build

# Single static binary. CGO is off because the SQLite driver is pure Go.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/cloudroof

# Providers are opt-out via build tags (no<name>): nohetzner nodo noaws
# noazure nogcp. `slim` keeps only the token-simple providers, which drops the
# heavy AWS/Azure/GCP SDKs and roughly quarters the binary.
SLIM_TAGS ?= noaws noazure nogcp
slim:
	CGO_ENABLED=0 go build -trimpath -tags "$(SLIM_TAGS)" -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/cloudroof

run: build
	$(BIN) -data ./data

# Backend with the frontend served from web/dist on disk; pair with `npm run dev`
# in web/ for hot reload (vite proxies /api to :7070).
dev:
	CLOUDROOF_LOG=debug go run ./cmd/cloudroof -dev -data ./data

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
			go build -trimpath -ldflags "$(LDFLAGS)" -o dist/cloudroof ./cmd/cloudroof; \
		tar -C dist -czf dist/cloudroof_$(VERSION)_$${os}_$${arch}.tar.gz cloudroof; \
		rm -f dist/cloudroof; \
	done
	@cd dist && sha256sum *.tar.gz > checksums.txt
	@echo "release $(VERSION):" && ls -1 dist

clean:
	rm -rf bin dist web/dist/* data
	touch web/dist/.gitkeep

docker:
	docker build -t cloudroof:$(VERSION) .
