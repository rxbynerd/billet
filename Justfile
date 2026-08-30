# Billet task runner. Run `just --list` for an overview.

# Build the billet binary into ./bin. CGO_ENABLED=0 so the result is a
# single static binary (confirmed statically linked when cross-compiled
# for linux; macOS binaries always carry a minimal libSystem link, which
# is a platform property, not evidence this flag had no effect).
build:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/billet ./cmd/billet

# Run all tests.
test:
    go test ./...

# Vet the whole module.
vet:
    go vet ./...

# Lint with golangci-lint if installed; fall back to go vet.
lint:
    @if command -v golangci-lint >/dev/null 2>&1; then \
        golangci-lint run ./...; \
    else \
        echo "golangci-lint not found; running go vet instead"; \
        go vet ./...; \
    fi

# Everything CI runs.
ci: build vet test lint
