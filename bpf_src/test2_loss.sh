#!/bin/bash
# TEST 2 - random loss: retransmission detection on a live iperf3 TCP
# flow under a real 2% netem loss rate.
#
# Expected capture difference, report but don't fail on it: eBPF
# counts retransmissions per GSO super-packet, so its count tends to
# run lower than iperf3's own reported Retr total; AF_PACKET observes
# egress after the qdisc and after GSO segmentation, so it never sees
# the netem-dropped originals at all, only their retransmissions.
#
# Usage: ./test2_loss.sh [-iface IFACE] [-capture ebpf|afpacket] [-runs N]
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=3
parse_common_args "$@"
trap cleanup_common EXIT

IPERF_TARGET="51.91.57.3"
overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test2_loss run $i/$RUNS (capture=$CAPTURE iface=$IFACE) ==="
	stop_glitch_exporter_if_needed
	log="/tmp/test2_${CAPTURE}_${i}.log"
	iperf_log="/tmp/test2_iperf3_${i}.log"

	sudo "$GLITCH_BIN" -iface "$IFACE" -capture "$CAPTURE" >"$log" 2>&1 &
	glitch_pid=$!
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 >"$iperf_log" 2>&1 &
	iperf_pid=$!
	sleep 2

	add_netem "loss 2%"

	wait "$iperf_pid" || true
	remove_netem
	sleep 1
	sudo kill -TERM "$glitch_pid" 2>/dev/null || true
	wait "$glitch_pid" 2>/dev/null || true
	restore_glitch_exporter

	retrans=$(grep -c "TCP Loss Detected" "$log" || true)
	stall_starts=$(grep -c "Stall START" "$log" || true)
	# iperf3's sender summary line ends "<Retr count>  sender" — the
	# second-to-last field on that line.
	iperf_retr=$(awk '/sender/{print $(NF-1)}' "$iperf_log" | tail -1)

	echo "run $i: our_retrans=$retrans iperf3_retr=${iperf_retr:-unknown} stall_starts=$stall_starts"
	if [ "$retrans" -gt 0 ]; then
		echo "run $i: PASS (retransmissions detected)"
	else
		echo "run $i: FAIL (no retransmissions detected despite 2% injected loss)"
		overall_pass=0
	fi
	echo "glitch-tool log: $log  iperf3 log: $iperf_log"
done

[ "$overall_pass" -eq 1 ]
