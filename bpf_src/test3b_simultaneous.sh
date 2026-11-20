#!/bin/bash
# TEST 3b (simultaneous) - server-side outage, eBPF and AF_PACKET
# glitch-tool running AT THE SAME TIME, watching the SAME single
# iperf3 connection, scored against the SAME real outage trigger.
#
# Why this exists, separate from test3b_server_outage.sh: that script
# runs each capture method as its own, separate invocation — its own
# iperf3 connection, its own SSH-triggered outage, at its own real
# time. That's enough to confirm each path detects a real outage on
# its own, but NOT enough to say the two paths agree with each other,
# since each run's own RTO-backoff timing (how far into its own
# retransmission-timer cycle the outage happened to land) can
# genuinely differ step-by-step between two separate TCP connections
# — see CLAUDE.md's "known limitations" on why episode duration/step
# count is timing-sensitive, not deterministic. This script removes
# that confound: both tools observe literally the same packets of the
# same connection during the same real outage, using their
# independently-scoped exclusivity locks (/var/run/glitch-<iface>.lock
# for eBPF, /var/run/glitch-afpacket-<iface>.lock for AF_PACKET — see
# glitch/afpacket's own doc comment) so both can attach to the same
# interface at once without contention.
#
# Pass per run: both logs individually pass score_outage.py's
# criterion (exactly one episode overlapping the outage window,
# duration > the outage length) on the SAME outage window, AND their
# reported episode timing is close enough to call "the same event" —
# start times within a generous tolerance (this is deliberately not
# a tight bound: AF_PACKET can lag eBPF by up to ~100ms due to
# TPACKET_V3's block-retire timeout, see CLAUDE.md, so anything
# beyond that plus a safety margin is the real signal worth flagging,
# not jitter).
#
# Usage: ./test3b_simultaneous.sh [-iface IFACE] [-runs N] [-outage-seconds N] [-parallel N]
# -parallel N runs iperf3 with N parallel streams (-P N, default 1) —
# a bigger aggregate burst for stress-testing AF_PACKET's drop
# counters against a bigger backlog than a single stream produces.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=3
parse_common_args "$@"
CAPTURE=ebpf # both backends run regardless of any -capture arg; forces stop_glitch_exporter_if_needed to actually free the eBPF lock below

if [ "$OUTAGE_SECONDS" -lt 1 ] || [ "$OUTAGE_SECONDS" -gt 10 ]; then
	echo "-outage-seconds must be 1-10 (iperf-outage's own limit)" >&2
	exit 1
fi

# AF_PACKET's own documented worst-case lag behind eBPF (block-retire
# timeout) plus a margin — see this script's header and CLAUDE.md.
# SSH_KEY/SSH_TARGET/SSH_OPTS/IPERF_TARGET and check_no_leftover_rule/
# trigger_outage are shared, see test_common.sh.
START_TOLERANCE_MS=250

EBPF_PID=""
AFPACKET_PID=""
stop_both() {
	if [ -n "$EBPF_PID" ]; then
		sudo kill -TERM "$EBPF_PID" 2>/dev/null || true
		wait "$EBPF_PID" 2>/dev/null || true
		EBPF_PID=""
	fi
	if [ -n "$AFPACKET_PID" ]; then
		sudo kill -TERM "$AFPACKET_PID" 2>/dev/null || true
		wait "$AFPACKET_PID" 2>/dev/null || true
		AFPACKET_PID=""
	fi
}
cleanup() {
	stop_both
	remove_netem
	restore_glitch_exporter
}
trap cleanup EXIT

overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test3b_simultaneous run $i/$RUNS (iface=$IFACE outage=${OUTAGE_SECONDS}s) ==="
	stop_glitch_exporter_if_needed

	# Log names carry OUTAGE_SECONDS/PARALLEL so consecutive invocations
	# with different flags (e.g. a 2s run then a 3s run) don't overwrite
	# each other's logs at the same index -- bit us the first time this
	# script was run with more than one configuration back to back.
	run_tag="o${OUTAGE_SECONDS}_p${PARALLEL}_${i}"
	ebpf_log="/tmp/test3b_sim_ebpf_${run_tag}.log"
	afpacket_log="/tmp/test3b_sim_afpacket_${run_tag}.log"
	iperf_log="/tmp/test3b_sim_iperf3_${run_tag}.log"

	sudo "$GLITCH_BIN" -iface "$IFACE" -capture ebpf >"$ebpf_log" 2>&1 &
	EBPF_PID=$!
	sudo "$GLITCH_BIN" -iface "$IFACE" -capture afpacket >"$afpacket_log" 2>&1 &
	AFPACKET_PID=$!
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 -P "$PARALLEL" >"$iperf_log" 2>&1 &
	iperf_pid=$!
	sleep 3

	echo "triggering server-side outage (${OUTAGE_SECONDS}s)..."
	read -r outage_start outage_end < <(trigger_outage "$OUTAGE_SECONDS")

	wait "$iperf_pid" || true
	sleep 1
	stop_both
	restore_glitch_exporter

	src_port=$(grep -oE 'port [0-9]+ connected' "$iperf_log" | head -1 | grep -oE '[0-9]+')
	iperf_retr=$(awk '/sender/{print $(NF-1)}' "$iperf_log" | tail -1)
	echo "run $i: iperf3 flow src_port=${src_port:-unknown} iperf3_retr=${iperf_retr:-unknown}"

	ebpf_retrans=$(grep "TCP Loss Detected" "$ebpf_log" | grep -c ":5201\$" || true)
	afpacket_retrans=$(grep "TCP Loss Detected" "$afpacket_log" | grep -c ":5201\$" || true)
	echo "run $i: ebpf_retrans=$ebpf_retrans afpacket_retrans=$afpacket_retrans iperf3_retr=${iperf_retr:-unknown}"

	echo "--- eBPF ($ebpf_log) ---"
	score_ebpf=$(python3 bpf_src/score_outage.py "$ebpf_log" ":5201" "$outage_start" "$outage_end" "$OUTAGE_SECONDS" || true)
	echo "$score_ebpf"

	echo "--- AF_PACKET ($afpacket_log) ---"
	score_afpacket=$(python3 bpf_src/score_outage.py "$afpacket_log" ":5201" "$outage_start" "$outage_end" "$OUTAGE_SECONDS" || true)
	echo "$score_afpacket"

	run_pass=1
	echo "$score_ebpf" | grep -q "^RESULT: PASS" || run_pass=0
	echo "$score_afpacket" | grep -q "^RESULT: PASS" || run_pass=0

	# Agreement check: both logs' one overlapping episode must start
	# within START_TOLERANCE_MS of each other — same real outage, same
	# real packets, so a bigger gap than AF_PACKET's own documented
	# worst-case lag is a real disagreement between the two paths, not
	# expected jitter.
	ebpf_start=$(echo "$score_ebpf" | grep "\[OVERLAPS\]" | grep -oE 'start=[0-9.]+' | head -1 | cut -d= -f2)
	afpacket_start=$(echo "$score_afpacket" | grep "\[OVERLAPS\]" | grep -oE 'start=[0-9.]+' | head -1 | cut -d= -f2)
	if [ -n "$ebpf_start" ] && [ -n "$afpacket_start" ]; then
		delta_ms=$(python3 -c "print(abs(($afpacket_start - $ebpf_start) * 1000))")
		echo "run $i: episode start delta (afpacket - ebpf) = ${delta_ms}ms (tolerance ${START_TOLERANCE_MS}ms)"
		if ! awk -v d="$delta_ms" -v t="$START_TOLERANCE_MS" 'BEGIN { exit !(d <= t) }'; then
			echo "run $i: DISAGREEMENT — episode start times differ by more than tolerance"
			run_pass=0
		fi
	else
		echo "run $i: cannot compare start times — one or both paths did not report a single overlapping episode (see FAIL reasons above)"
		run_pass=0
	fi

	if [ "$run_pass" -eq 1 ]; then
		echo "run $i: PASS (both paths agree on the same real outage)"
	else
		echo "run $i: FAIL"
		overall_pass=0
	fi
	echo "ebpf log: $ebpf_log  afpacket log: $afpacket_log  iperf3 log: $iperf_log"

	check_no_leftover_rule
	echo "(confirmed: no leftover 5201 rule on wservices)"
done

[ "$overall_pass" -eq 1 ]
