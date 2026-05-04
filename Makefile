.PHONY: build build-web build-go build-linux test lint run dev snapshot deb deploy release-dry-run clean

GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.1")
VERSION ?= $(shell echo "$(GIT_VERSION)" | grep -qE '^[0-9]' && echo "$(GIT_VERSION)" || echo "0.0.0-$(GIT_VERSION)")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

DEPLOY_HOST ?=
DEB_NAME    = grosz_$(VERSION)_amd64.deb

build-web:
	cd web && npm ci && npm run build

build: build-web
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o grosz ./cmd/grosz

build-go:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o grosz ./cmd/grosz

build-linux: build-web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o grosz-linux-amd64 ./cmd/grosz

test:
	go test -race -count=1 ./cmd/... ./internal/... ./testutil/...

lint:
	golangci-lint run ./...

run: build-go
	./grosz

dev:
	@echo "Starting Go server and Vite dev server..."
	@trap 'kill 0' EXIT; \
		go run ./cmd/grosz & \
		cd web && npm run dev & \
		wait

snapshot: build-web
	goreleaser release --snapshot --clean --skip=publish,docker

deb: snapshot
	@cp dist/grosz_*_linux_amd64.deb $(DEB_NAME)
	@echo "Built $(DEB_NAME)"

deploy: deb
	@test -n "$(DEPLOY_HOST)" || { echo "Set DEPLOY_HOST=user@host (e.g. in ~/.zshrc or .envrc)"; exit 1; }
	scp $(DEB_NAME) $(DEPLOY_HOST):/tmp/$(DEB_NAME)
	ssh $(DEPLOY_HOST) "dpkg -i /tmp/$(DEB_NAME) && rm /tmp/$(DEB_NAME)"
	ssh $(DEPLOY_HOST) "systemctl restart grosz"
	@echo "Deployed $(DEB_NAME) to $(DEPLOY_HOST)"

release-dry-run: build-web
	goreleaser release --snapshot --clean

clean:
	rm -f grosz grosz-linux-amd64
	rm -f grosz_*.deb
	rm -rf dist/
	rm -rf web/dist/
	rm -rf web/node_modules/
