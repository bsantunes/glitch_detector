#!/bin/bash
# TEST 3b - server-side outage: same as test3_outage.sh, but the
# outage is real (iptables DROP on wservices, triggered over SSH),
# not netem on the gateway — so both -capture ebpf and -capture
# afpacket genuinely see the same thing (real data and
# retransmissions leave the gateway and never get ACKed; a local
# netem outage drops packets at the gateway itself, which test3's own
# AF_PACKET warning covers). Same pass criteria as test3_outage.sh ("at least
# one" matching stall, not "exactly one" — see that script's own
# header for why), valid for both capture methods.
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
IPERF_TARGET="51.91.57.3"

# Fails loudly rather than letting a leftover DROP rule on a shared,
# production server (it also runs InfluxDB/Grafana for the whole
# fleet, and Nagios) go unnoticed between runs.
check_no_leftover_rule() {
	local rules
	if ! rules=$(ssh -i "$SSH_KEY" "$SSH_TARGET" check5201 2>&1); then
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

	sudo "$GLITCH_BIN" -iface "$IFACE" -capture "$CAPTURE" >"$log" 2>&1 &
	glitch_pid=$!
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 >/dev/null 2>&1 &
	iperf_pid=$!
	sleep 3

	echo "triggering server-side outage (${OUTAGE_SECONDS}s)..."
	ssh -i "$SSH_KEY" "$SSH_TARGET" "outage $OUTAGE_SECONDS"

	wait "$iperf_pid" || true
	sleep 1
	sudo kill -TERM "$glitch_pid" 2>/dev/null || true
	wait "$glitch_pid" 2>/dev/null || true
	restore_glitch_exporter

	# :5201 at end of line — the dst port position in glitch-tool's
	# "SrcIP:SrcPort -> DstIP:DstPort" output, and iperf3's server
	# port, so this scopes every count to the actual test flow.
	starts=$(grep "Stall START" "$log" | grep -c ":5201$" || true)
	ends=$(grep "Stall END" "$log" | grep -c ":5201$" || true)
	retrans=$(grep "TCP Loss Detected" "$log" | grep -c ":5201$" || true)

	echo "run $i: stall_starts=$starts stall_ends=$ends retrans=$retrans"
	grep "Stall END" "$log" | grep ":5201$" || true

	# At least one Stall END attributable to the injected outage
	# (duration >= the outage length) — see test3_outage.sh's header
	# for why "at least one" rather than "exactly one": a real
	# cellular link can produce its own short, legitimate stall from
	# ordinary jitter during a bulk transfer, unrelated to the
	# injected outage.
	outage_ms=$((OUTAGE_SECONDS * 1000))
	matched=0
	while read -r dur; do
		[ -z "$dur" ] && continue
		if awk -v d="$dur" -v o="$outage_ms" 'BEGIN { exit !(d >= o) }'; then
			matched=$((matched + 1))
		fi
	done < <(grep "Stall END" "$log" | grep ":5201$" | grep -oE 'duration [0-9.]+ms' | grep -oE '[0-9.]+')

	if [ "$matched" -ge 1 ] && [ "$retrans" -gt 0 ]; then
		if [ "$ends" -gt "$matched" ]; then
			echo "run $i: PASS (outage matched by $matched stall(s) >= ${outage_ms}ms; $((ends - matched)) additional shorter stall(s) also seen — incidental link jitter, unrelated to the injected outage)"
		else
			echo "run $i: PASS"
		fi
	else
		echo "run $i: FAIL (expected at least one Stall END >= ${outage_ms}ms on the iperf3 flow, plus retransmissions)"
		overall_pass=0
	fi
	echo "log: $log"

	check_no_leftover_rule
	echo "(confirmed: no leftover 5201 rule on wservices)"
done

[ "$overall_pass" -eq 1 ]
