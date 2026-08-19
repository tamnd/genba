BIN     := genbad
PKG     := ./cmd/genbad
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/tamnd/genba.Version=$(VERSION) \
	-X github.com/tamnd/genba.Commit=$(COMMIT) \
	-X github.com/tamnd/genba.Date=$(DATE)

export CGO_ENABLED := 0

.PHONY: build cli install test race cover bench vet fmt lint tidy vuln headless clean run

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) $(PKG)

cli:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/genba ./cmd/genba

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/...

test:
	go test ./...

race:
	go test -race -count=1 ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

bench:
	go test -run '^$$' -bench . -benchmem ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run

tidy:
	go mod tidy

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Build the API only server, without the browser interface compiled in.
headless:
	go build -trimpath -tags noassets -ldflags "$(LDFLAGS)" -o bin/$(BIN) $(PKG)

clean:
	rm -rf bin coverage.out

run: build
	./bin/$(BIN)
