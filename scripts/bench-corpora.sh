#!/usr/bin/env bash
# Reproducible real-repository evidence for repath (issue #61).
#
#   make bench-corpora
#   ./scripts/bench-corpora.sh [--runs N] [--corpora a,b] [--out DIR]
#                              [--no-clone] [--update-baseline]
#
# What it does:
#   1. resolves the pinned corpus matrix below (repo + commit + language +
#      optional SCIP producer), cloning a missing clone under .cache/ and
#      refusing a clone whose HEAD or tracked tree does not match;
#   2. runs TestBenchRealLanguages N times per corpus in separate processes
#      (so the Linux VmHWM reading belongs to one tree) and writes one JSON
#      per run under <out>/runs/;
#   3. runs the hermetic failure-mode test and records its exit codes;
#   4. runs the opt-in SCIP accuracy test for the corpora marked `accuracy`;
#   5. aggregates everything into <out>/summary.json (host/CPU/RAM/load, tool
#      versions, medians+ranges, graph structure, adapter stats, accuracy) and
#      prints deltas against bench/baseline.json.
#
# It is evidence, never a CI gate, and needs no producer for `make check`:
# missing clones are cloned, missing SCIP files skip only the adapter half.
# The default output directory is under the gitignored .cache/. Only
# --update-baseline rewrites the two tracked files bench/baseline.json and
# docs/benchmarks-history.md.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

RUNS=3
OUTDIR=""
CORPORA="django,hyusk,requests,rayon"
NO_CLONE=0
UPDATE_BASELINE=0

usage() {
	cat <<'EOF'
usage: scripts/bench-corpora.sh [options]

  --runs N             runs per corpus in separate processes (default 3)
  --corpora a,b        subset of django,hyusk,requests,rayon
  --out DIR            output directory (default .cache/bench-corpora/<utc>)
  --no-clone           fail when a corpus clone is missing instead of cloning
  --update-baseline    rewrite bench/baseline.json and docs/benchmarks-history.md
  -h, --help           this text

Per-corpus overrides (paths):
  REPATH_BENCH_CORPUS_ROOT_<NAME>   e.g. REPATH_BENCH_CORPUS_ROOT_DJANGO
  REPATH_BENCH_CORPUS_SCIP_<NAME>   e.g. REPATH_BENCH_CORPUS_SCIP_DJANGO
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--runs)
		RUNS=${2:?--runs needs a number}
		shift 2
		;;
	--corpora)
		CORPORA=${2:?--corpora needs a comma-separated list}
		shift 2
		;;
	--out)
		OUTDIR=${2:?--out needs a directory}
		shift 2
		;;
	--no-clone)
		NO_CLONE=1
		shift
		;;
	--update-baseline)
		UPDATE_BASELINE=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "bench-corpora: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

command -v go >/dev/null 2>&1 || export PATH=/usr/local/go/bin:$PATH
command -v go >/dev/null 2>&1 || {
	echo "bench-corpora: go not found (expected /usr/local/go/bin on this machine)" >&2
	exit 2
}
command -v git >/dev/null 2>&1 || {
	echo "bench-corpora: git not found" >&2
	exit 2
}
command -v python3 >/dev/null 2>&1 || {
	echo "bench-corpora: python3 is required for summary aggregation" >&2
	exit 2
}

# The sandbox home is read-only; keep the Go cache in the workspace unless the
# caller already chose one (the same environment docs/development.md documents).
export GOCACHE=${GOCACHE:-$ROOT/.cache/go-build}
export GOPATH=${GOPATH:-$ROOT/.cache/gopath}
export GOMODCACHE=${GOMODCACHE:-$ROOT/.cache/gomod}
export CCACHE_DISABLE=1

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
if [ -z "$OUTDIR" ]; then
	OUTDIR="$ROOT/.cache/bench-corpora/$STAMP"
fi
mkdir -p "$OUTDIR/runs" "$OUTDIR/logs"
OUTDIR=$(cd "$OUTDIR" && pwd)
case "$OUTDIR" in
"$ROOT/.cache/"*) ;;
*) echo "bench-corpora: warning: output outside .cache/: $OUTDIR" >&2 ;;
esac

die() {
	echo "bench-corpora: $*" >&2
	exit 1
}

LOAD_BEFORE=$(cat /proc/loadavg 2>/dev/null || true)

# corpus_spec prints: language|repo|commit|default-root|default-scip|accuracy
corpus_spec() {
	case "$1" in
	django)
		echo "python|https://github.com/django/django|f24f38c6b2825f15628c07bf2892f1664eed9361|.cache/django-60|.cache/django-60-prod.scip|0"
		;;
	hyusk)
		echo "rust|https://github.com/Goutam-11/hyusk|06bdb2b15fcc0a0f7dc3f2b06e0003dc1731cb82|.cache/hyusk-60|.cache/hyusk-60.scip|0"
		;;
	requests)
		echo "python|https://github.com/psf/requests|3c7baaa05ab4aa49cc819ce3a819d260de19c157|.cache/requests|.cache/requests.scip|1"
		;;
	rayon)
		echo "rust|https://github.com/rayon-rs/rayon|599425d4a4c64714632817f48496dbc53f656a90|.cache/rayon|.cache/rayon.scip|1"
		;;
	*)
		return 1
		;;
	esac
}

verify_commit() {
	local root=$1 commit=$2
	local head
	head=$(git -C "$root" rev-parse HEAD)
	[ "$head" = "$commit" ] || die "$root HEAD = $head, want pinned $commit"
}

verify_clean() {
	local root=$1 dirty
	# .repath/ is a runtime artifact and deliberately ignored by the harness.
	dirty=$(git -C "$root" status --porcelain | grep -v -E '^.. \.repath/' || true)
	[ -z "$dirty" ] || die "$root is dirty (use a throwaway clean clone):
$dirty"
}

prepare_clone() {
	local root=$1 repo=$2 commit=$3
	if [ -d "$root/.git" ]; then
		return 0
	fi
	if [ -e "$root" ] && [ -n "$(ls -A "$root" 2>/dev/null)" ]; then
		die "$root exists but is not a git clone; move it away or set REPATH_BENCH_CORPUS_ROOT_*"
	fi
	[ "$NO_CLONE" = 1 ] && die "missing clone $root (--no-clone)"
	echo "bench-corpora: cloning $repo -> $root"
	mkdir -p "$(dirname "$root")"
	git clone --quiet --depth 1 "$repo" "$root" || die "clone $repo failed"
	local head
	head=$(git -C "$root" rev-parse HEAD)
	if [ "$head" != "$commit" ]; then
		echo "bench-corpora: fetching pinned $commit"
		git -C "$root" fetch --quiet --depth 1 origin "$commit" ||
			die "fetch $commit from $repo failed (the server may refuse SHA fetches)"
		git -C "$root" checkout --quiet --detach FETCH_HEAD || die "checkout $commit failed"
	fi
}

rm -f "$OUTDIR/matrix.tsv"
IFS=',' read -ra NAMES <<<"$CORPORA"
[ "${#NAMES[@]}" -gt 0 ] || die "empty --corpora"
for name in "${NAMES[@]}"; do
	spec=$(corpus_spec "$name") || die "unknown corpus '$name' (want django,hyusk,requests,rayon)"
	IFS='|' read -r lang repo commit default_root default_scip accuracy <<<"$spec"

	upper=$(echo "$name" | tr '[:lower:]' '[:upper:]')
	root_var="REPATH_BENCH_CORPUS_ROOT_${upper}"
	scip_var="REPATH_BENCH_CORPUS_SCIP_${upper}"
	root=${!root_var:-$ROOT/$default_root}
	scip=${!scip_var:-$ROOT/$default_scip}

	prepare_clone "$root" "$repo" "$commit"
	verify_commit "$root" "$commit"
	verify_clean "$root"

	if [ ! -f "$scip" ]; then
		echo "bench-corpora: warning: SCIP missing for $name ($scip); adapter half and accuracy will be skipped" >&2
		scip=""
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$name" "$lang" "$repo" "$commit" "$root" "$scip" "$accuracy" >>"$OUTDIR/matrix.tsv"

	# The Go harness reads REPATH_BENCH_PY_* / REPATH_BENCH_RS_*.
	case "$lang" in
	python) lang_prefix=PY ;;
	rust) lang_prefix=RS ;;
	*) die "unsupported language '$lang' for $name" ;;
	esac
	lang_root_var="REPATH_BENCH_${lang_prefix}_ROOT"
	lang_commit_var="REPATH_BENCH_${lang_prefix}_COMMIT"
	lang_scip_var="REPATH_BENCH_${lang_prefix}_SCIP"

	for i in $(seq 1 "$RUNS"); do
		json="$OUTDIR/runs/${name}-${i}.json"
		log="$OUTDIR/logs/${name}-${i}.log"
		echo "bench-corpora: run $name [$i/$RUNS] ($lang)"
		if ! env \
			REPATH_BENCH_LANGS=1 REPATH_BENCH_LANG="$lang" \
			"$lang_root_var=$root" "$lang_commit_var=$commit" "$lang_scip_var=$scip" \
			REPATH_BENCH_CORPUS="$name" REPATH_BENCH_LANGUAGE="$lang" \
			REPATH_BENCH_JSON="$json" \
			go test ./internal/index -run '^TestBenchRealLanguages$' -v -count=1 \
			>"$log" 2>&1; then
			tail -n 40 "$log" >&2
			die "run $name [$i/$RUNS] failed (log $log)"
		fi
		[ -s "$json" ] || {
			tail -n 40 "$log" >&2
			die "run $name [$i/$RUNS] wrote no JSON (log $log)"
		}
	done

	if [ "$accuracy" = 1 ] && [ -n "$scip" ]; then
		accuracy_json="$OUTDIR/accuracy-${name}.json"
		accuracy_log="$OUTDIR/logs/accuracy-${name}.log"
		echo "bench-corpora: accuracy $name ($lang)"
		if ! env \
			REPATH_ACCURACY=1 REPATH_ACCURACY_LANG="$lang" REPATH_ACCURACY_CORPUS="$name" \
			REPATH_ACCURACY_ROOT="$root" REPATH_ACCURACY_SCIP="$scip" \
			REPATH_ACCURACY_COMMIT="$commit" REPATH_ACCURACY_JSON="$accuracy_json" \
			go test ./internal/adapter -run '^TestSCIPAccuracy$' -v -count=1 \
			>"$accuracy_log" 2>&1; then
			tail -n 40 "$accuracy_log" >&2
			die "accuracy $name failed (log $accuracy_log)"
		fi
	fi
done

echo "bench-corpora: failure modes"
if ! env REPATH_FAILURE_JSON="$OUTDIR/failure-modes.json" \
	go test ./internal/index -run '^TestCorporaFailureModes$' -v -count=1 \
	>"$OUTDIR/failure-modes.txt" 2>&1; then
	tail -n 40 "$OUTDIR/failure-modes.txt" >&2
	die "failure-mode test failed (log $OUTDIR/failure-modes.txt)"
fi

repath_commit=$(git -C "$ROOT" rev-parse HEAD)
repath_version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)

summary_args=(
	--outdir "$OUTDIR"
	--runs "$RUNS"
	--repath-commit "$repath_commit"
	--repath-version "$repath_version"
	--baseline "$ROOT/bench/baseline.json"
	--history "$ROOT/docs/benchmarks-history.md"
)
if [ "$UPDATE_BASELINE" = 1 ]; then
	summary_args+=(--update-baseline)
fi
LOAD_AFTER=$(cat /proc/loadavg 2>/dev/null || true)
REPATH_BENCH_LOAD_BEFORE="$LOAD_BEFORE" REPATH_BENCH_LOAD_AFTER="$LOAD_AFTER" \
	python3 "$ROOT/scripts/bench-corpora-summary.py" "${summary_args[@]}"

echo "bench-corpora: logs under $OUTDIR/logs"
