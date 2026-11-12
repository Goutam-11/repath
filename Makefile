VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Goutam-11/repath/internal/cli.version=$(VERSION)

.PHONY: build test vet fmt check clean perf perf-10k perf-50k bench-real release-snapshot

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/repath ./cmd/repath

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check:
	test -z "$$(gofmt -l ./cmd ./internal)" || (echo "unformatted files above; run 'make fmt'"; exit 1)
	go vet ./...
	go test ./...

# Reproducible synthetic budgets (PLAN §10); not part of make check. Each run is
# a separate process so the Linux VmHWM reading belongs to that tree alone.
perf: perf-10k perf-50k

perf-10k:
	REPATH_PERF=1 go test ./internal/index -run '^TestPerf10k$$' -v -count=1

perf-50k:
	REPATH_PERF=1 go test ./internal/index -run '^TestPerf50k$$' -v -count=1

# Opt-in real-repository evidence (issue #31), never a CI gate. Point
# REPATH_BENCH_ROOT at a clean clone of the pinned repo in docs/benchmarks.md;
# the test removes .repath/ and briefly touches ten source files, restoring them.
bench-real:
	@test -n "$(REPATH_BENCH_ROOT)" || (echo "set REPATH_BENCH_ROOT=/path/to/clean/pinned/clone (see docs/benchmarks.md)"; exit 2)
	REPATH_BENCH=1 REPATH_BENCH_ROOT="$(REPATH_BENCH_ROOT)" REPATH_BENCH_COMMIT="$(REPATH_BENCH_COMMIT)" go test ./internal/index -run '^TestBenchReal$$' -v -count=1

# Local snapshot of the current platform. Requires goreleaser >= 2.20 on PATH;
# the tag-triggered release workflow builds both amd64 and arm64.
release-snapshot:
	goreleaser build --snapshot --single-target --clean

clean:
	rm -rf bin
