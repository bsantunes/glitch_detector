#!/bin/bash
# TEST 3 - gateway-side outage: a short total outage (netem loss 100%
# on this interface) must be reported as a stall episode on the
# iperf3 flow (port 5201) whose duration is at least the outage length
# itself (RTO backoff — see CLAUDE.md's glitch/ package section's
# "known limitations" — always makes the real one longer than the
# outage, never shorter). Reference results (pinned deps, -capture
# ebpf, aaeon, 3/3 pass, -outage-seconds 1): 1160.7 ms/3 steps,
# 1276.0 ms/3 steps, 2118.4 ms/4 steps.
#
# Pass requires exactly one episode OVERLAPPING the outage's own
# wall-clock window (start before the outage ends, end after it
# starts), with duration > the outage length — scored by
# score_outage.py against glitch-tool's now-timestamped event lines.
# A stall fully outside that window (confirmed live: a real cellular
# link can produce its own short, legitimate stall purely from
# ordinary jitter during a bulk transfer, completely unrelated to the
# injected outage — one run saw a 111ms/1-step episode fire before
# netem was even added) is reported but doesn't affect the verdict.
# Two or more episodes overlapping the window is always a FAIL — that
# is the outage having been wrongly split into two episodes, which a
# bare "duration >= outage" check can't tell apart from one correctly-
# detected episode.
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

	start_glitch_tool "$log"
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 >/dev/null 2>&1 &
	iperf_pid=$!
	sleep 3

	outage_start=$(date -u +%s.%N)
	add_netem "loss 100%"
	sleep "$OUTAGE_SECONDS"
	remove_netem
	outage_end=$(date -u +%s.%N)

	wait "$iperf_pid" || true
	sleep 1
	stop_glitch_tool
	restore_glitch_exporter

	# :5201 at end of line — that's the dst port position in
	# glitch-tool's own "SrcIP:SrcPort -> DstIP:DstPort" output, and
	# iperf3's server port, so this scopes stall counts to the actual
	# test flow rather than any other traffic the gateway produces.
	# retrans can no longer be scoped the same way: both capture
	# backends count retransmissions directly (glitch.Source.
	# RetransCount) instead of emitting a per-flow Event for each one,
	# specifically so they stop competing for a slot in the same channel
	# a real burst's stall events share — see glitch-detector-internals
	# skill. retrans below is therefore this whole run's total across
	# every flow glitch-tool saw, not just the iperf3 one.
	starts=$(grep "Stall START" "$log" | grep -c ":5201$" || true)
	ends=$(grep "Stall END" "$log" | grep -c ":5201$" || true)
	retrans=$(grep -oE "retrans=[0-9]+" "$log" | tail -1 | cut -d= -f2)
	retrans=${retrans:-0}

	echo "run $i: stall_starts=$starts stall_ends=$ends retrans=$retrans"
	grep "Stall END" "$log" | grep ":5201$" || true

	if [ "$CAPTURE" = "afpacket" ]; then
		echo "run $i: NOT SCORED (afpacket, see warning above)"
		continue
	fi

	score_out=$(python3 bpf_src/score_outage.py "$log" ":5201" "$outage_start" "$outage_end" "$OUTAGE_SECONDS" || true)
	echo "$score_out"
	if echo "$score_out" | grep -q "^RESULT: PASS" && [ "$retrans" -gt 0 ]; then
		echo "run $i: PASS"
	else
		echo "run $i: FAIL"
		overall_pass=0
	fi
	echo "log: $log"
done

if [ "$CAPTURE" != "afpacket" ]; then
	[ "$overall_pass" -eq 1 ]
fi
