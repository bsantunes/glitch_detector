#!/bin/bash
# Shared helpers for bpf_src/test{1,2,3,3b}_*.sh — not a standalone
# script, sourced by each (`source bpf_src/test_common.sh`). Common
# argument parsing (-iface/-capture/-runs[/-outage-seconds]),
# glitch-exporter stop/restart, and netem qdisc add/remove — both
# registered via the caller's own `trap cleanup_common EXIT` so a
# failed run never leaves glitch-exporter down or a netem qdisc
# attached. One shared implementation rather than four near-identical
# copies, specifically because this project's own history is full of
# subtle bugs from exactly this kind of duplicated-but-slightly-
# different cleanup logic — one fix here fixes it everywhere.

IFACE=wwan0
CAPTURE=ebpf
RUNS=1
OUTAGE_SECONDS=1
GLITCH_BIN="${GLITCH_TOOL_BIN:-./glitch-tool}"

parse_common_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		-iface)
			IFACE="$2"
			shift 2
			;;
		-capture)
			CAPTURE="$2"
			shift 2
			;;
		-runs)
			RUNS="$2"
			shift 2
			;;
		-outage-seconds)
			OUTAGE_SECONDS="$2"
			shift 2
			;;
		*)
			echo "unknown option: $1" >&2
			exit 1
			;;
		esac
	done
	case "$CAPTURE" in
	ebpf | afpacket) ;;
	*)
		echo "-capture must be ebpf or afpacket, got $CAPTURE" >&2
		exit 1
		;;
	esac
}

GLITCH_EXPORTER_STOPPED=0
# -capture ebpf attaches to the same clsact qdisc glitch-exporter's
# own eBPF path uses, and glitch.Open's exclusivity lock refuses a
# second attach to the same interface — so glitch-exporter has to be
# stopped first. -capture afpacket takes its own, separate lock (see
# glitch/afpacket.Open's doc comment) and never conflicts with
# glitch-exporter at all, so this is a deliberate no-op for it.
stop_glitch_exporter_if_needed() {
	if [ "$CAPTURE" = "ebpf" ] && systemctl is-active --quiet glitch-exporter 2>/dev/null; then
		echo "stopping glitch-exporter (holds the tc lock on $IFACE for -capture ebpf)..."
		sudo systemctl stop glitch-exporter
		GLITCH_EXPORTER_STOPPED=1
	fi
}
restore_glitch_exporter() {
	if [ "$GLITCH_EXPORTER_STOPPED" = "1" ]; then
		echo "restarting glitch-exporter..."
		sudo systemctl start glitch-exporter || echo "WARNING: failed to restart glitch-exporter — check it manually" >&2
		GLITCH_EXPORTER_STOPPED=0
	fi
}

NETEM_ADDED=0
add_netem() {
	# $1 = netem args, e.g. "loss 2%" or "loss 100%"
	# shellcheck disable=SC2086
	sudo tc qdisc add dev "$IFACE" root netem $1
	NETEM_ADDED=1
}
remove_netem() {
	if [ "$NETEM_ADDED" = "1" ]; then
		sudo tc qdisc del dev "$IFACE" root 2>/dev/null || true
		NETEM_ADDED=0
	fi
}

# GLITCH_PID / start_glitch_tool / stop_glitch_tool — every script
# backgrounds glitch-tool via start_glitch_tool and must tear it down
# via stop_glitch_tool, not a one-off inline `sudo kill` at the
# "normal" end of its loop body. Confirmed live why this matters: an
# early script abort (e.g. the SSH call in test3b failing before its
# own manual kill/wait ever ran) left two root-owned glitch-tool
# processes running indefinitely, each still holding its own
# exclusivity lock -- invisible until the *next* invocation failed
# with "another glitch monitor is already attached," and in the
# meantime both kept writing into whatever log path they'd been
# redirected to, long after the script that started them had already
# exited, corrupting a later run's own log file (a shell's `>`
# redirection truncates the existing inode in place; the orphaned
# process's own file descriptor, opened earlier at a far larger
# offset, then resumes writing from *its* cursor, leaving a block of
# NUL bytes in between). Routing every start/stop through here, under
# the same trap cleanup_common EXIT every script already registers,
# closes that gap: no code path can finish without either normally
# stopping glitch-tool or hitting the trap that does it instead.
GLITCH_PID=""
start_glitch_tool() {
	# $1 = log file path
	sudo "$GLITCH_BIN" -iface "$IFACE" -capture "$CAPTURE" >"$1" 2>&1 &
	GLITCH_PID=$!
}
stop_glitch_tool() {
	if [ -n "$GLITCH_PID" ]; then
		sudo kill -TERM "$GLITCH_PID" 2>/dev/null || true
		wait "$GLITCH_PID" 2>/dev/null || true
		GLITCH_PID=""
	fi
}

cleanup_common() {
	stop_glitch_tool
	remove_netem
	restore_glitch_exporter
}
