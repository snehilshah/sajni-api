# sajni-api — Go backend (deployed to Cloud Run).

-include .env
export

.PHONY: help dev build run fmt lint check test docker-build docker-run clean sync-vars

help:
	@echo "sajni-api targets:"
	@echo "  dev           run the server with go run"
	@echo "  build         compile a static binary -> ./sajni"
	@echo "  run           build and run the binary"
	@echo "  fmt           gofmt -w ."
	@echo "  lint          gofmt -l + go vet (read-only)"
	@echo "  check         what CI runs: lint + build + test"
	@echo "  test          go test ./..."
	@echo "  docker-build  build the Cloud Run image (sajni-api:dev)"
	@echo "  docker-run    docker-build then run with .env"
	@echo "  sync-vars     push secrets.txt -> GitHub Actions variables (requires gh)"

# --- dev ---
dev:
	go run ./cmd

# --- build (no Docker) ---
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sajni ./cmd

run: build
	./sajni

# --- format ---
fmt:
	gofumpt -w .

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

docker-run: docker-build
	docker run --rm -p 8080:8080 --env-file .env sajni-api:dev

# --- GitHub Actions variable sync ---
# Reads secrets.txt (KEY=VALUE) and syncs each line to GitHub Actions
# variables via `gh`. Requires: gh auth login + repo write access.
# Use for non-sensitive config (vars.*). Sensitive secrets live in GCP
# Secret Manager and are never stored here.
sync-vars:
	@echo "Syncing GitHub Actions variables from secrets.txt..."
	@grep -v '^[[:space:]]*#' secrets.txt | grep '=' | while IFS= read -r line; do \
		key=$${line%%=*}; val=$${line#*=}; \
		gh variable set "$$key" --body "$$val" && echo "  set $$key"; \
	done
	@echo "Done."

# --- cleanup ---
clean:
	rm -f sajni
	rm -rf data/blobs
