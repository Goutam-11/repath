VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Goutam-11/repath/internal/cli.version=$(VERSION)

.PHONY: build test vet fmt check clean perf perf-10k perf-50k

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

clean:
	rm -rf bin
