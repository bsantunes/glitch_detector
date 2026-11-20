#!/bin/bash
# TEST 3b (exporter load) - load-test the actual production binary
# (glitch-exporter -capture afpacket), not the glitch-tool diagnostic
# CLI test3b_simultaneous.sh uses, under a real outage big enough to
# stress both AF_PACKET's capture and this exporter's own InfluxDB
# write path - including that write path failing at the same moment
# as the burst, which is the realistic case for a real radio outage:
# this device's own InfluxDB writes and the traffic being monitored
# share the same uplink (see CLAUDE.md's "Known gotcha" on
# glitch-exporter). Confirmed by reading influxdb-client-go/v2's own
# source before writing this: writeAPI.WritePoint sends on unbuffered
# channels all the way down to the goroutine doing the actual HTTP
# write, so a stuck write can block it for as long as that write takes
# to fail - glitch-exporter's own event-consumer loop now goes through
# a bounded queue instead of calling WritePoint directly, specifically
# so this can't starve h.Events() the way it used to be able to (see
# cmd/glitch-exporter/main.go's own comment on writeQueue).
#
# Per run, reports: iperf3 retr, KernelDropped/UserDropped/
# UntruncatedOverLimit (afpacket.Capture's own breakdown, read back via
# the glitch_summary points this test's own glitch-exporter instance
# writes - not from its log, which only logs when something is
# nonzero), and whether the run's glitch_summary/glitch_event points
# actually land in InfluxDB once writes resume, with the real kernel
# timestamps (not the late arrival time). Pass per run: kernel_dropped
# and user_dropped both sum to zero over the run's own window.
#
# Needs: glitch-exporter built (make glitch-exporter), root (via sudo,
# interactively - this script prompts, it is not meant to run
# unattended), and /opt/dawn/modem-exporter.env (the same InfluxDB
# token production's glitch-exporter.service already uses - read via a
# small generated, root-only helper script so the token never sits in
# this script's own environment, argv, or output). Stops
# glitch-exporter.service for the run (same reasoning as
# test3b_simultaneous.sh's eBPF-lock stop — CAPTURE stays the default
# "ebpf" specifically to make stop_glitch_exporter_if_needed fire
# unconditionally here), restarts it after.
#
# Usage: ./test3b_exporter_load.sh [-iface IFACE] [-runs N] [-outage-seconds N] [-parallel N]
# Run bare, no leading sudo — see glitch-detector-internals skill's
# sudo-wrapping pitfall note, same reasoning applies here.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=test_common.sh
source bpf_src/test_common.sh

RUNS=3
parse_common_args "$@"

if [ "$OUTAGE_SECONDS" -lt 1 ] || [ "$OUTAGE_SECONDS" -gt 10 ]; then
	echo "-outage-seconds must be 1-10 (iperf-outage's own limit)" >&2
	exit 1
fi

EXPORTER_BIN="${GLITCH_EXPORTER_BIN:-./glitch-exporter}"
if [ ! -x "$EXPORTER_BIN" ]; then
	echo "FATAL: $EXPORTER_BIN not found or not executable — run 'make glitch-exporter' first" >&2
	exit 1
fi
EXPORTER_BIN="$(readlink -f "$EXPORTER_BIN")"

ENV_FILE="/opt/dawn/modem-exporter.env"
if ! sudo test -r "$ENV_FILE"; then
	echo "FATAL: $ENV_FILE not readable even via sudo" >&2
	exit 1
fi

INFLUX_URL="${TEST_INFLUX_URL:-https://wservices.wavesys.pt:8086}"
INFLUX_ORG="${TEST_INFLUX_ORG:-wavecom}"
INFLUX_BUCKET="${TEST_INFLUX_BUCKET:-network}"
TEST_DEVICE="${TEST_DEVICE_ID:-aaeon-afpacket-loadtest}"
INFLUX_DPORT=8086

# Two tiny generated helper scripts, root-run via sudo, so the real
# InfluxDB token is sourced only inside a root process and never
# touches this script's own environment, argv (visible to other local
# users via ps), or output.
EXPORTER_LAUNCH_SCRIPT="$(mktemp)"
QUERY_SCRIPT="$(mktemp)"
cat >"$EXPORTER_LAUNCH_SCRIPT" <<SCRIPT
#!/bin/bash
source "$ENV_FILE"
exec "$EXPORTER_BIN" -iface "$IFACE" -capture afpacket -interval 2s -influx-url "$INFLUX_URL" -influx-org "$INFLUX_ORG" -influx-bucket "$INFLUX_BUCKET" -device-id "$TEST_DEVICE"
SCRIPT
cat >"$QUERY_SCRIPT" <<SCRIPT
#!/bin/bash
source "$ENV_FILE"
curl -s -XPOST "$INFLUX_URL/api/v2/query?org=$INFLUX_ORG" -H "Authorization: Token \${INFLUX_TOKEN}" -H "Accept: application/csv" -H "Content-type: application/vnd.flux" --data-binary @-
SCRIPT
chmod 700 "$EXPORTER_LAUNCH_SCRIPT" "$QUERY_SCRIPT"

query_influx() { # reads a Flux query on stdin, prints CSV on stdout
	sudo bash "$QUERY_SCRIPT"
}
csv_field() { # $1=csv $2=field name -> prints that field's _value (first match)
	# InfluxDB's v2 query API returns "annotated CSV": a few #-prefixed
	# metadata lines, THEN the real header (containing _field/_value as
	# column names, not on line 1) — and a fresh annotation+header block
	# per result table. Detect the header by content, not position, and
	# re-detect it every time it recurs, rather than assuming NR==1.
	echo "$1" | awk -F, -v want="$2" '
		/^#/ { next }
		/_field/ && /_value/ {
			fcol = 0; vcol = 0
			for (i = 1; i <= NF; i++) { if ($i == "_field") fcol = i; if ($i == "_value") vcol = i }
			next
		}
		fcol && vcol && $0 != "" && $fcol == want { print $vcol; exit }
	'
}

EXPORTER_PID=""
start_test_exporter() { # $1 = log file path
	sudo bash "$EXPORTER_LAUNCH_SCRIPT" >"$1" 2>&1 &
	EXPORTER_PID=$!
}
stop_test_exporter() {
	if [ -n "$EXPORTER_PID" ]; then
		sudo kill -TERM "$EXPORTER_PID" 2>/dev/null || true
		wait "$EXPORTER_PID" 2>/dev/null || true
		EXPORTER_PID=""
	fi
}

IPTABLES_RULE_ADDED=0
block_influx_writes() {
	sudo iptables -I OUTPUT -o "$IFACE" -p tcp --dport "$INFLUX_DPORT" -j DROP
	IPTABLES_RULE_ADDED=1
}
unblock_influx_writes() {
	if [ "$IPTABLES_RULE_ADDED" = "1" ]; then
		sudo iptables -D OUTPUT -o "$IFACE" -p tcp --dport "$INFLUX_DPORT" -j DROP
		IPTABLES_RULE_ADDED=0
	fi
}

cleanup() {
	stop_test_exporter
	unblock_influx_writes
	restore_glitch_exporter
	rm -f "$EXPORTER_LAUNCH_SCRIPT" "$QUERY_SCRIPT"
}
trap cleanup EXIT

overall_pass=1
for i in $(seq 1 "$RUNS"); do
	echo "=== test3b_exporter_load run $i/$RUNS (iface=$IFACE outage=${OUTAGE_SECONDS}s parallel=$PARALLEL) ==="
	stop_glitch_exporter_if_needed

	run_tag="o${OUTAGE_SECONDS}_p${PARALLEL}_${i}"
	exporter_log="/tmp/test3b_exload_exporter_${run_tag}.log"
	iperf_log="/tmp/test3b_exload_iperf3_${run_tag}.log"

	start_test_exporter "$exporter_log"
	sleep 2

	iperf3 -c "$IPERF_TARGET" --bind-dev "$IFACE" -t 10 -P "$PARALLEL" >"$iperf_log" 2>&1 &
	iperf_pid=$!
	sleep 3

	echo "blocking this device's own InfluxDB writes (dport $INFLUX_DPORT on $IFACE) for the outage window..."
	block_influx_writes
	echo "triggering server-side outage (${OUTAGE_SECONDS}s)..."
	read -r outage_start outage_end < <(trigger_outage "$OUTAGE_SECONDS")
	unblock_influx_writes
	echo "InfluxDB writes unblocked (outage window: ${outage_start}..${outage_end})"

	wait "$iperf_pid" || true
	sleep 2
	stop_test_exporter
	restore_glitch_exporter

	iperf_retr=$(awk '/sender/{print $(NF-1)}' "$iperf_log" | tail -1)
	echo "run $i: iperf3_retr=${iperf_retr:-unknown}"

	echo "--- exporter log ($exporter_log) ---"
	grep -E "drops this interval|influx async write error" "$exporter_log" || echo "(nothing logged — either clean, or the interval tick hasn't fired yet; the InfluxDB query below is authoritative)"

	check_no_leftover_rule
	echo "(confirmed: no leftover 5201 rule on wservices)"

	echo "waiting 15s for any delayed retries to flush before querying InfluxDB..."
	sleep 15

	query_start=$(python3 -c "import datetime; print(datetime.datetime.utcfromtimestamp($outage_start-5).strftime('%Y-%m-%dT%H:%M:%SZ'))")
	query_stop=$(python3 -c "import datetime; print(datetime.datetime.utcfromtimestamp($outage_end+30).strftime('%Y-%m-%dT%H:%M:%SZ'))")

	summary_csv=$(query_influx <<EOF
from(bucket: "$INFLUX_BUCKET")
  |> range(start: $query_start, stop: $query_stop)
  |> filter(fn: (r) => r._measurement == "glitch_summary")
  |> filter(fn: (r) => r.device == "$TEST_DEVICE")
  |> filter(fn: (r) => r._field == "dropped_events" or r._field == "kernel_dropped_events" or r._field == "user_dropped_events" or r._field == "untruncated_over_limit_events" or r._field == "write_queue_dropped_events")
  |> sum()
EOF
	)
	echo "--- glitch_summary field sums over the run window ($query_start..$query_stop) ---"
	echo "$summary_csv" | awk -F, '
		/^#/ { next }
		/_field/ && /_value/ {
			fcol = 0; vcol = 0
			for (i = 1; i <= NF; i++) { if ($i == "_field") fcol = i; if ($i == "_value") vcol = i }
			next
		}
		fcol && vcol && $0 != "" { print $fcol"="$vcol }
	'

	event_csv=$(query_influx <<EOF
from(bucket: "$INFLUX_BUCKET")
  |> range(start: $query_start, stop: $query_stop)
  |> filter(fn: (r) => r._measurement == "glitch_event")
  |> filter(fn: (r) => r.device == "$TEST_DEVICE")
  |> filter(fn: (r) => r._field == "duration_ms")
  |> keep(columns: ["_time", "_value", "type"])
EOF
	)
	echo "--- glitch_event points (stall_start/end/abandoned), kernel timestamp + duration_ms ---"
	echo "$event_csv"

	kernel_dropped=$(csv_field "$summary_csv" "kernel_dropped_events")
	user_dropped=$(csv_field "$summary_csv" "user_dropped_events")
	untrunc=$(csv_field "$summary_csv" "untruncated_over_limit_events")
	wqd=$(csv_field "$summary_csv" "write_queue_dropped_events")
	echo "run $i: kernel_dropped=${kernel_dropped:-?} user_dropped=${user_dropped:-?} untruncated_over_limit=${untrunc:-?} write_queue_dropped=${wqd:-?}"

	run_pass=1
	if [ -z "$kernel_dropped" ] || [ -z "$user_dropped" ]; then
		echo "run $i: could not read kernel/user dropped sums from InfluxDB (query may have failed, or no summary point landed in the window) — treating as FAIL"
		run_pass=0
	elif ! awk -v k="$kernel_dropped" -v u="$user_dropped" 'BEGIN{exit !(k==0 && u==0)}'; then
		echo "run $i: NONZERO drops"
		run_pass=0
	fi
	if [ -z "$event_csv" ] || [ "$(echo "$event_csv" | wc -l)" -lt 2 ]; then
		echo "run $i: no glitch_event points found in the window — the outage may not have produced a reported stall, or points never arrived"
		run_pass=0
	fi

	if [ "$run_pass" -eq 1 ]; then
		echo "run $i: PASS (zero kernel/user drops, points present in InfluxDB)"
	else
		echo "run $i: FAIL"
		overall_pass=0
	fi
	echo "exporter log: $exporter_log  iperf3 log: $iperf_log"
done

[ "$overall_pass" -eq 1 ]
