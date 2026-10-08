#!/usr/bin/env bash
#
# Run the performance benchmarks, and optionally hold them to a recorded
# baseline.
#
#   scripts/bench.sh            run the benchmarks, print the results
#   scripts/bench.sh --update   run them and overwrite the baseline
#   scripts/bench.sh --check    run them and fail if anything regressed
#
# Why a baseline at all: there were already a few benchmarks in core, but
# nothing ever compared two runs, so a regression could only be noticed by
# someone who happened to remember the old number. That is exactly how a state
# root costing a full fold over the ledger survived for months. Correctness
# tests use tiny chains, so a term that grows with the ledger never appears in
# one; a recorded number does not have that blind spot.
#
# The threshold is deliberately loose. This runs on developer machines, not a
# quiet CI box, so the signal worth catching is "this got several times slower",
# not "this got 10% slower" — a tighter bound would mean a failure every time
# something else on the machine woke up.
set -euo pipefail

cd "$(dirname "$0")/.."

GO=${GO:-go}
BENCHTIME=${BENCHTIME:-50x}
BASELINE=${BASELINE:-benchmarks/baseline.txt}
# A benchmark must be this many times slower than the baseline to fail.
THRESHOLD=${THRESHOLD:-2.5}

mode=run
case "${1:-}" in
--update) mode=update ;;
--check) mode=check ;;
"") ;;
*)
	echo "usage: $0 [--update|--check]" >&2
	exit 2
	;;
esac

out=$(mktemp)
trap 'rm -f "$out"' EXIT

echo "==== running benchmarks (benchtime=$BENCHTIME) ===="
# -run XXX so no correctness test runs: this measures, it does not verify.
$GO test ./core/ -run XXX -bench . -benchtime "$BENCHTIME" -benchmem |
	grep -E '^Benchmark' | tee "$out"

normalize() {
	# name -> ns/op, dropping the -N worker suffix so a run on a machine with a
	# different core count still compares.
	sed -E 's/-[0-9]+[[:space:]]+/ /' "$1" | awk '{print $1, $3}'
}

case $mode in
update)
	mkdir -p "$(dirname "$BASELINE")"
	{
		echo "# DNAS benchmark baseline — ns/op, recorded by scripts/bench.sh --update"
		echo "# host: $(uname -s)/$(uname -m)  go: $($GO version | awk '{print $3}')"
		echo "# These are a REGRESSION TRIPWIRE, not a spec. Re-record deliberately."
		normalize "$out"
	} >"$BASELINE"
	echo
	echo "baseline written to $BASELINE"
	;;
check)
	if [ ! -f "$BASELINE" ]; then
		echo "no baseline at $BASELINE; run $0 --update first" >&2
		exit 1
	fi
	echo
	echo "==== comparing against $BASELINE (threshold ${THRESHOLD}x) ===="
	normalize "$out" >"$out.now"
	grep -v '^#' "$BASELINE" >"$out.base" || true
	rc=0
	while read -r name base; do
		[ -n "$name" ] || continue
		now=$(awk -v n="$name" '$1==n {print $2}' "$out.now")
		if [ -z "$now" ]; then
			echo "MISSING  $name (in the baseline, not in this run)"
			rc=1
			continue
		fi
		verdict=$(awk -v b="$base" -v n="$now" -v t="$THRESHOLD" \
			'BEGIN { r = (b > 0) ? n / b : 1; printf "%s %.2f", (r > t ? "SLOWER" : "ok"), r }')
		ratio=${verdict#* }
		if [ "${verdict%% *}" = "SLOWER" ]; then
			echo "SLOWER   $name  ${base}ns -> ${now}ns  (${ratio}x)"
			rc=1
		else
			printf 'ok       %s  %sns -> %sns  (%sx)\n' "$name" "$base" "$now" "$ratio"
		fi
	done <"$out.base"
	rm -f "$out.now" "$out.base"
	if [ $rc -ne 0 ]; then
		echo
		echo "benchmarks regressed past ${THRESHOLD}x. If the change is intended, re-record with $0 --update." >&2
	fi
	exit $rc
	;;
esac
