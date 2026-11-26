VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Goutam-11/repath/internal/cli.version=$(VERSION) \
           -X github.com/Goutam-11/repath/internal/index.BuildRevision=$(VERSION)

.PHONY: build test vet fmt check hooks hooks-check clean perf perf-10k perf-50k bench-real bench-real-langs bench-corpora release-snapshot

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/repath ./cmd/repath

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check:
	test -z "$$(gofmt -l ./cmd ./internal ./skills)" || (echo "unformatted files above; run 'make fmt'"; exit 1)
	go vet ./...
	go test ./...
	./scripts/test-commit-msg-hook.sh

# Install the commit-msg guard for this clone (local config, so a fresh clone or
# machine needs it again). It refuses a bare <username>@users.noreply.github.com
# identity or trailer, which GitHub can attribute to a different account.
hooks:
	git config core.hooksPath .githooks
	@echo "installed core.hooksPath=.githooks; a refusal is bypassed with --no-verify"

hooks-check:
	./scripts/test-commit-msg-hook.sh

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

# Opt-in Python/Rust evidence (issue #48), never a CI gate. Each corpus runs in
# its own process so the VmHWM reading belongs to that tree. SCIP is optional;
# without it the matrix still runs but the adapter before/after pass is skipped.
# Opt-in token-savings evidence (issue #87), never a CI gate: compare the tokens
# of the answers with the tokens of the files they name. Set
# REPATH_TOKENS_CORPORA="requests=.cache/requests,django=.cache/django-60".
bench-tokens:
	@test -n "$(REPATH_TOKENS_CORPORA)" || (echo "set REPATH_TOKENS_CORPORA=name=path[,name=path] (see docs/benchmarks.md)"; exit 2)
	REPATH_TOKENS=1 REPATH_TOKENS_CORPORA="$(REPATH_TOKENS_CORPORA)" go test ./internal/eval -run '^TestAnswerTokens$$' -v -count=1

bench-real-langs:
	@test -n "$(REPATH_BENCH_PY_ROOT)$(REPATH_BENCH_RS_ROOT)" || (echo "set REPATH_BENCH_PY_ROOT and/or REPATH_BENCH_RS_ROOT (see docs/benchmarks.md)"; exit 2)
	REPATH_BENCH_LANGS=1 REPATH_BENCH_LANG=python \
		REPATH_BENCH_PY_ROOT="$(REPATH_BENCH_PY_ROOT)" REPATH_BENCH_PY_COMMIT="$(REPATH_BENCH_PY_COMMIT)" REPATH_BENCH_PY_SCIP="$(REPATH_BENCH_PY_SCIP)" \
		go test ./internal/index -run '^TestBenchRealLanguages$$' -v -count=1
	REPATH_BENCH_LANGS=1 REPATH_BENCH_LANG=rust \
		REPATH_BENCH_RS_ROOT="$(REPATH_BENCH_RS_ROOT)" REPATH_BENCH_RS_COMMIT="$(REPATH_BENCH_RS_COMMIT)" REPATH_BENCH_RS_SCIP="$(REPATH_BENCH_RS_SCIP)" \
		go test ./internal/index -run '^TestBenchRealLanguages$$' -v -count=1

# Reproducible corpus evidence (issue #61), never a CI gate. Runs the pinned
# django/hyusk/requests/rayon matrix N=3 in separate processes, writes
# <outdir>/summary.json plus accuracy and failure-mode JSON, and prints deltas
# against bench/baseline.json. ARGS passes flags through, e.g.
#   make bench-corpora ARGS="--runs 1 --corpora requests"
# Only `--update-baseline` rewrites the checked-in baseline/history.
bench-corpora:
	./scripts/bench-corpora.sh $(ARGS)

# Local snapshot of the current platform. Requires goreleaser >= 2.20 on PATH;
# the tag-triggered release workflow builds both amd64 and arm64.
release-snapshot:
	goreleaser build --snapshot --single-target --clean

clean:
	rm -rf bin
