#!/bin/bash
# TEST 3b - server-side outage: same as test3_outage.sh, but the
# outage is real (iptables DROP on wservices, triggered over SSH),
# not netem on the gateway — so both -capture ebpf and -capture
# afpacket genuinely see the same thing (real data and
# retransmissions leave the gateway and never get ACKed; a local
# netem outage drops packets at the gateway itself, which test3's own
# AF_PACKET warning covers). Same pass criteria as test3_outage.sh
# (exactly one episode overlapping the outage's own wall-clock
# window, duration > the outage length — see that script's own
# header), valid for both capture methods. Also reports iperf3's own
# Retr total for this run, for comparison against our own retrans
# count (see CLAUDE.md's glitch/ package section on GSO coalescing).
#
# wservices side (already provisioned there, not this script's job):
#   /usr/local/sbin/iperf-outage [seconds] — inserts
#   `iptables -I INPUT -p tcp --dport 5201 -j DROP`, sleeps, deletes
#   the rule (DROP, not REJECT; port 5201 only — that host also runs
#   InfluxDB/Grafana). Refuses to run without root. sudoers grants
#   idwave NOPASSWD on exactly that command.
#
# Requires a dedicated SSH key restricted, on the wservices side, to
# running exactly two things as idwave — the outage script itself,
# and a read-only check for a leftover dport-5201 rule — nothing
# else. See CLAUDE.md's glitch/ package section ("test3b SSH key
# setup") for how that key and its restriction were provisioned.
#
# Usage: ./test3b_server_outage.sh [-iface IFACE] [-capture ebpf|afpacket] [-runs N] [-outage-seconds N]
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=3
parse_common_args "$@"
trap cleanup_common EXIT

if [ "$OUTAGE_SECONDS" -lt 1 ] || [ "$OUTAGE_SECONDS" -gt 10 ]; then
	echo "-outage-seconds must be 1-10 (iperf-outage's own limit)" >&2
	exit 1
fi

SSH_KEY="${TEST3B_SSH_KEY:-$HOME/.ssh/id_ed25519_wservices_outage}"
SSH_TARGET="${TEST3B_SSH_TARGET:-idwave@wservices.wavesys.pt}"
# BatchMode: never block waiting for a password/passphrase this
# key will never have one of; accept-new: trust a host key seen
# for the first time without an interactive prompt (still rejects
# a *changed* key, same as the default) -- confirmed live that the
# interactive version of this prompt can stall the script for an
# arbitrary amount of real time, well past when the already-running
# iperf3/glitch-tool background processes finish on their own,
# silently invalidating the whole run.
SSH_OPTS="-o BatchMode=yes -o StrictHostKeyChecking=accept-new"
IPERF_TARGET="51.91.57.3"

# Fails loudly rather than letting a leftover DROP rule on a shared,
# production server (it also runs InfluxDB/Grafana for the whole
# fleet, and Nagios) go unnoticed between runs.
check_no_leftover_rule() {
	local rules
	if ! rules=$(ssh $SSH_OPTS -i "$SSH_KEY" "$SSH_TARGET" check5201 2>&1); then
		echo "FATAL: could not check wservices for a leftover iptables rule: $rules" >&2
		exit 1
	fi
	if echo "$rules" | grep -q 5201; then
		echo "FATAL: a dport-5201 DROP rule is still present on wservices after the outage — iperf-outage did not clean up. Stopping, not safe to continue." >&2
		echo "$rules" >&2
		exit 1
	fi
}

overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test3b_server_outage run $i/$RUNS (capture=$CAPTURE iface=$IFACE outage=${OUTAGE_SECONDS}s) ==="
	stop_glitch_exporter_if_needed
	log="/tmp/test3b_${CAPTURE}_${i}.log"

	iperf_log="/tmp/test3b_iperf3_${CAPTURE}_${i}.log"

	start_glitch_tool "$log"
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 >"$iperf_log" 2>&1 &
	iperf_pid=$!
	sleep 3

	outage_start=$(date -u +%s.%N)
	echo "triggering server-side outage (${OUTAGE_SECONDS}s)..."
	ssh $SSH_OPTS -i "$SSH_KEY" "$SSH_TARGET" "outage $OUTAGE_SECONDS"
	outage_end=$(date -u +%s.%N)

	wait "$iperf_pid" || true
	sleep 1
	stop_glitch_tool
	restore_glitch_exporter

	# :5201 at end of line — the dst port position in glitch-tool's
	# "SrcIP:SrcPort -> DstIP:DstPort" output, and iperf3's server
	# port, so this scopes stall counts to the actual test flow. retrans
	# can't be scoped the same way any more - both capture backends count
	# retransmissions directly instead of emitting a per-flow Event for
	# each one (see glitch-detector-internals skill), so it's this run's
	# total across every flow, not just the iperf3 one.
	starts=$(grep "Stall START" "$log" | grep -c ":5201$" || true)
	ends=$(grep "Stall END" "$log" | grep -c ":5201$" || true)
	retrans=$(grep -oE "retrans=[0-9]+" "$log" | tail -1 | cut -d= -f2)
	retrans=${retrans:-0}

	echo "run $i: stall_starts=$starts stall_ends=$ends retrans=$retrans"
	grep "Stall END" "$log" | grep ":5201$" || true

	# iperf3's sender summary line ends "<Retr count>  sender" — the
	# second-to-last field on that line. Reported for comparison
	# against our own retrans count (see test2_loss.sh and CLAUDE.md's
	# GSO-coalescing note).
	iperf_retr=$(awk '/sender/{print $(NF-1)}' "$iperf_log" | tail -1)
	echo "run $i: iperf3_retr=${iperf_retr:-unknown}"

	score_out=$(python3 bpf_src/score_outage.py "$log" ":5201" "$outage_start" "$outage_end" "$OUTAGE_SECONDS" || true)
	echo "$score_out"
	if echo "$score_out" | grep -q "^RESULT: PASS" && [ "$retrans" -gt 0 ]; then
		echo "run $i: PASS"
	else
		echo "run $i: FAIL"
		overall_pass=0
	fi
	echo "glitch-tool log: $log  iperf3 log: $iperf_log"

	check_no_leftover_rule
	echo "(confirmed: no leftover 5201 rule on wservices)"
done

[ "$overall_pass" -eq 1 ]
