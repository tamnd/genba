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

.PHONY: build cli install test race cover bench bench-corpus bench-search bench-counters bench-gate bench-gate-record ui-gate vet fmt lint tidy vuln headless clean run

# How large the benchmark corpus is. The budgets are stated against a hundred
# thousand documents, which takes a couple of minutes to generate, so the
# default here is the smaller corpus people actually run while they work.
BENCH_DOCS ?= 20000
BENCH_SEED ?= 2121

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

# The benchmark corpus, which is generated rather than checked in. It is a few
# hundred megabytes and it is derived entirely from the seed, so anybody who
# runs this gets the same documents and the numbers stay comparable. The
# benchmarks build it on demand too, and this target is for building it once up
# front rather than inside a timed run.
bench-corpus:
	go run ./benchcorpus/gen -seed $(BENCH_SEED) -n $(BENCH_DOCS) -out testdata/bench-$(BENCH_SEED)-$(BENCH_DOCS).db

# The endpoint, search and driver benchmarks, against that corpus. Three levels
# of the same corpus, so the difference between two of the numbers is the cost of
# the layer between them.
bench-search:
	GENBA_BENCH_DOCS=$(BENCH_DOCS) go test -run '^$$' -bench . -benchmem ./api/ ./index/ ./store/sqlitestore/

# The performance gate, which is what runs on every pull request.
#
# It asserts work rather than time: how many rows a query reads, how many
# statements it runs, how many documents it decodes. A shared CI runner cannot
# measure a millisecond and it can count rows exactly, so this fails on a change
# that makes a query read the corpus and stays quiet when the runner is busy.
bench-counters:
	go test -count=1 -run 'Counters' ./index/

# The other half of the gate, which is the wall clock.
#
# The counters cannot see a change that does the same work more slowly, and this
# can. It measures the search endpoint per query class in interleaved rounds,
# compares the median of the quietest round against benchcorpus/baseline.json,
# and normalises by a calibration workload so a slow runner is not read as a
# slow query. It writes its numbers to bench-report.json, which CI keeps as an
# artifact.
bench-gate:
	GENBA_BENCH_DOCS=$(BENCH_DOCS) GENBA_GATE_REPORT=$(CURDIR)/bench-report.json go test -v -count=1 -timeout 30m -run 'LatencyGate' ./api/

# Record a new baseline for the machine this runs on.
#
# It belongs in a commit of its own that changes nothing else, with a message
# saying why the number moved. A baseline that arrives alongside the change it
# is measuring is a baseline nobody can review, and one that updates itself is a
# ratchet that only turns one way.
bench-gate-record:
	GENBA_BENCH_DOCS=$(BENCH_DOCS) GENBA_GATE_RECORD=1 GENBA_GATE_RUNNER="$(shell uname -sm)" GENBA_GATE_DATE="$(shell date -u +%Y-%m-%d)" \
		go test -v -count=1 -timeout 30m -run 'LatencyGate' ./api/

# The browser gate: asset budgets, markup safety, axe, and Lighthouse.
#
# The Go tests are the part that never flakes and they run first. The browser
# half needs npx and a Chrome, and says so and stops rather than failing when
# there is not one.
ui-gate: build
	go test -count=1 -run 'Interface|Assets|Cache' ./web/
	./scripts/ui-gate.sh

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
