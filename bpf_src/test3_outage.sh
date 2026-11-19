#!/bin/bash
# TEST 3 - gateway-side outage: a short total outage (netem loss 100%
# on this interface) must be reported as exactly one stall episode on
# the iperf3 flow (port 5201), with a duration somewhat longer than
# the outage itself (RTO backoff — see CLAUDE.md's glitch/ package
# section's "known limitations" on why). Reference results (pinned
# deps, -capture ebpf, aaeon, 3/3 pass, -outage-seconds 1): 1160.7 ms/
# 3 steps, 1276.0 ms/3 steps, 2118.4 ms/4 steps.
#
# Valid for -capture ebpf only: AF_PACKET observes egress after the
# qdisc, so the netem-dropped originals never reach its tap at all —
# this script warns and skips PASS/FAIL scoring for -capture afpacket
# rather than reporting a false FAIL. Use test3b_server_outage.sh (a
# real server-side outage, which both capture methods genuinely see)
# to compare the two.
#
# Usage: ./test3_outage.sh [-iface IFACE] [-capture ebpf|afpacket] [-runs N] [-outage-seconds N]
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=3
parse_common_args "$@"
trap cleanup_common EXIT

if [ "$CAPTURE" = "afpacket" ]; then
	echo "WARNING: test3 (netem on the gateway itself) is not comparable for -capture afpacket — the netem-dropped originals never reach the AF_PACKET tap at all, only their retransmissions do. Proceeding for informational output only; not scoring PASS/FAIL. Use test3b_server_outage.sh to compare the two capture methods on an outage." >&2
fi

IPERF_TARGET="51.91.57.3"
overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test3_outage run $i/$RUNS (capture=$CAPTURE iface=$IFACE outage=${OUTAGE_SECONDS}s) ==="
	stop_glitch_exporter_if_needed
	log="/tmp/test3_${CAPTURE}_${i}.log"

	sudo "$GLITCH_BIN" -iface "$IFACE" -capture "$CAPTURE" >"$log" 2>&1 &
	glitch_pid=$!
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 >/dev/null 2>&1 &
	iperf_pid=$!
	sleep 3

	add_netem "loss 100%"
	sleep "$OUTAGE_SECONDS"
	remove_netem

	wait "$iperf_pid" || true
	sleep 1
	sudo kill -TERM "$glitch_pid" 2>/dev/null || true
	wait "$glitch_pid" 2>/dev/null || true
	restore_glitch_exporter

	# :5201 at end of line — that's the dst port position in
	# glitch-tool's own "SrcIP:SrcPort -> DstIP:DstPort" output, and
	# iperf3's server port, so this scopes every count to the actual
	# test flow rather than any other traffic the gateway produces.
	starts=$(grep "Stall START" "$log" | grep -c ":5201$" || true)
	ends=$(grep "Stall END" "$log" | grep -c ":5201$" || true)
	retrans=$(grep "TCP Loss Detected" "$log" | grep -c ":5201$" || true)

	echo "run $i: stall_starts=$starts stall_ends=$ends retrans=$retrans"
	grep "Stall END" "$log" | grep ":5201$" || true

	if [ "$CAPTURE" = "afpacket" ]; then
		echo "run $i: NOT SCORED (afpacket, see warning above)"
		continue
	fi
	if [ "$starts" -eq 1 ] && [ "$ends" -eq 1 ] && [ "$retrans" -gt 0 ]; then
		echo "run $i: PASS"
	else
		echo "run $i: FAIL (expected exactly 1 Stall START + 1 Stall END on the iperf3 flow, plus retransmissions)"
		overall_pass=0
	fi
	echo "log: $log"
done

if [ "$CAPTURE" != "afpacket" ]; then
	[ "$overall_pass" -eq 1 ]
fi
