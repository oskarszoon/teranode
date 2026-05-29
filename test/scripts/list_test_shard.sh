#!/bin/bash
# Usage: list_test_shard.sh <pkg_dir> <shard> <total> [skip_regex]
# Prints an anchored -run regex (e.g. ^(TestA|TestB)$) selecting this shard's tests.
set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: $0 <pkg_dir> <shard> <total> [skip_regex]" >&2
    exit 1
fi

PKG="$1"
SHARD="$2"
TOTAL="$3"
SKIP="${4:-}"

if ! [ "$TOTAL" -gt 0 ] 2>/dev/null; then
    echo "Error: <total> must be a positive integer (got '$TOTAL')" >&2
    exit 1
fi
if ! { [ "$SHARD" -ge 0 ] && [ "$SHARD" -lt "$TOTAL" ]; } 2>/dev/null; then
    echo "Error: <shard> must be an integer in [0, $TOTAL) (got '$SHARD')" >&2
    exit 1
fi

# Enumerate with the SAME build config the smoketest run uses (no -tags) so the
# listed set matches what actually executes — otherwise a tag-gated test could be
# distributed to a shard's -run regex but never compiled/run, silently dropping it.
mapfile -t tests < <(cd "$PKG" && go test -list '.*' 2>/dev/null | grep '^Test' | sort)

sel=()
i=0
for t in "${tests[@]}"; do
    if [ -n "$SKIP" ] && [[ "$t" =~ $SKIP ]]; then continue; fi
    if [ $(( i % TOTAL )) -eq "$SHARD" ]; then sel+=("$t"); fi
    i=$((i+1))
done

if [ ${#sel[@]} -eq 0 ]; then
    echo '^$'
    exit 0
fi

printf '^(%s)$\n' "$(IFS='|'; echo "${sel[*]}")"
