# sajni-api — Go backend (deployed to Cloud Run).

-include .env
LOG_LEVEL ?= debug
export

.PHONY: help setup dev build run fmt lint check test docker-build docker-run clean sync-vars

help:
	@echo "sajni-api targets:"
	@echo "  setup         prepare the local .env once (safe to rerun)"
	@echo "  dev           run the server with go run"
	@echo "  build         compile a static binary -> ./sajni"
	@echo "  run           build and run the binary"
	@echo "  fmt           gofmt -w ."
	@echo "  lint          gofmt -l + go vet (read-only)"
	@echo "  check         what CI runs: lint + build + test"
	@echo "  test          go test ./..."
	@echo "  docker-build  build the Cloud Run image (sajni-api:dev)"
	@echo "  docker-run    docker-build then run with .env"
	@echo "  sync-vars     push github-variables.env -> GitHub Actions variables"

# --- dev ---
setup:
	@if [ ! -f .env ]; then cp .env.example .env; echo "Created .env from .env.example"; fi
	@if ! grep -q '^APP_ENV=' .env; then printf '\nAPP_ENV=local\n' >> .env; fi
	@if grep -Eq '^JWT_SECRET=(|change-me-to-a-long-random-string)$$' .env; then \
		secret="$$(openssl rand -hex 32)"; \
		sed -i "s|^JWT_SECRET=.*|JWT_SECRET=$$secret|" .env; \
		echo "Generated local JWT_SECRET"; \
	fi
	@chmod 600 .env
	@echo "Local environment ready"

dev: setup
	go run ./cmd

# --- build (no Docker) ---
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sajni ./cmd

run: build
	./sajni

# --- format ---
fmt:
	gofmt -w .

# --- lint (read-only) ---
lint:
	@unformatted="$$(gofmt -l .)"; \
	  if [ -n "$$unformatted" ]; then \
	    echo "gofmt found unformatted files (run 'make fmt'):"; \
	    echo "$$unformatted"; exit 1; \
	  fi
	go vet ./...

# --- CI gate ---
check: lint
	go build ./...
	go test ./...

# --- tests ---
test:
	go test ./...

# --- Docker (matches what CI builds) ---
docker-build:
	docker build -t sajni-api:dev .

docker-run: setup docker-build
	docker run --rm -p 8080:8080 --env-file .env sajni-api:dev

# --- GitHub Actions variable sync ---
# Reads github-variables.env (KEY=VALUE) and syncs each line to GitHub Actions
# variables via `gh`. Requires: gh auth login + repo write access.
# Use for non-sensitive config (vars.*). Sensitive secrets live in GCP
# Secret Manager and are never stored here.
sync-vars:
	@echo "Syncing GitHub Actions variables from github-variables.env..."
	@grep -v '^[[:space:]]*#' github-variables.env | grep '=' | while IFS= read -r line; do \
		key=$${line%%=*}; val=$${line#*=}; \
		gh variable set "$$key" --body "$$val" && echo "  set $$key"; \
	done
	@echo "Done."

# --- cleanup ---
clean:
	rm -f sajni
	rm -rf data/blobs
