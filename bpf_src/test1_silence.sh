#!/bin/bash
# TEST 1 - silence: idle and request-reply traffic (the gateway's own
# normal background — WireGuard, InfluxDB writes, TWAMP — with no
# injected test traffic at all) must never be reported as a stall.
#
# Usage: ./test1_silence.sh [-iface IFACE] [-capture ebpf|afpacket] [-runs N]
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=1 # default for test 1 specifically, overridable via -runs
parse_common_args "$@"
trap cleanup_common EXIT

overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test1_silence run $i/$RUNS (capture=$CAPTURE iface=$IFACE) ==="
	stop_glitch_exporter_if_needed
	log="/tmp/test1_${CAPTURE}_${i}.log"

	sudo "$GLITCH_BIN" -iface "$IFACE" -capture "$CAPTURE" >"$log" 2>&1 &
	glitch_pid=$!
	sleep 60
	sudo kill -TERM "$glitch_pid" 2>/dev/null || true
	wait "$glitch_pid" 2>/dev/null || true
	restore_glitch_exporter

	starts=$(grep -c "Stall START" "$log" || true)
	retrans=$(grep -c "TCP Loss Detected" "$log" || true)

	echo "run $i: stall_starts=$starts retrans=$retrans"
	if [ "$starts" -eq 0 ]; then
		echo "run $i: PASS"
	else
		echo "run $i: FAIL (background traffic was reported as a stall)"
		overall_pass=0
	fi
	echo "log: $log"
done

[ "$overall_pass" -eq 1 ]
