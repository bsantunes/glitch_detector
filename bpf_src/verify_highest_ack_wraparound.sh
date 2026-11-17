#!/bin/bash
# Regression test for the highest_ack-signed-comparison bug (see the
# "highest_ack must start relative to this connection's own sequence
# space" comment in tc_glitch.bpf.c's new-flow branch).
#
# TCP ISNs are randomized across the full 32-bit range, so roughly half
# of all real connections have seq/ack values that sit at or above
# 2^31 for their whole practical lifetime (actual transfer sizes never
# approach the 4 GiB wrap). Before the fix, highest_ack started at a
# bare 0: for exactly those connections, (s32)(real_ack - 0) is always
# negative, so ingress_monitor's "did the ack advance" check never saw
# it as an advance, and independently egress_monitor's own eligibility
# check (s32)(end_seq - highest_ack) also came out negative purely
# because end_seq itself is >= 2^31 - permanently disabling stall
# detection for that half of all connections. This script drives a
# synthetic flow entirely inside that region (seq starting at
# 3,000,000,000, well above 2^31) through the loaded, live BPF
# programs via bpftool prog run, and checks that a stall is still
# correctly detected.
#
# Requires: an instance of glitch-tool (or dawn-agent/glitch-exporter)
# already attached to some interface, so egress_monitor/ingress_monitor
# are loaded and flow_tracker is live. Run as root. Reads that
# instance's own printed output (or redirect its stdout to a file
# first and tail it) to see the result - this script only injects
# packets, it does not read events itself.
#
# Usage: sudo ./verify_highest_ack_wraparound.sh
# Then check the attached glitch-tool's output for:
#   🟡 [KERNEL] Stall START ... | TCP 10.9.9.1:51000 -> 10.9.9.2:5999
# If that line is missing, the bug has regressed.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (bpftool prog run needs it)" >&2
    exit 1
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

python3 - "$WORKDIR" <<'PYEOF'
import struct, sys
workdir = sys.argv[1]

SRC = (10, 9, 9, 1)
DST = (10, 9, 9, 2)
SPORT, DPORT = 51000, 5999
BASE_SEQ = 3_000_000_000  # > 2**31 = 2147483648 - the region the bug lived in

def build_egress(seq, payload_len, ack=1):
    ip_ver_ihl = (4 << 4) | 5
    tot_len = 20 + 20 + payload_len
    ip_hdr = struct.pack('!BBHHHBBH4s4s',
        ip_ver_ihl, 0, tot_len, 0, 0, 64, 6, 0,
        bytes(SRC), bytes(DST))
    doff_flags = (5 << 12) | 0x18  # doff=5, PSH+ACK
    tcp_hdr = struct.pack('!HHIIHHHH',
        SPORT, DPORT, seq, ack, doff_flags, 502, 0, 0)
    return ip_hdr + tcp_hdr

def build_ingress(ack_seq):
    # src/dst swapped: this packet's src is the remote peer (DST),
    # dst is us (SRC) - matches ingress_monitor's own key-swap logic.
    ip_ver_ihl = (4 << 4) | 5
    tot_len = 20 + 20  # pure ACK, no payload
    ip_hdr = struct.pack('!BBHHHBBH4s4s',
        ip_ver_ihl, 0, tot_len, 0, 0, 64, 6, 0,
        bytes(DST), bytes(SRC))
    doff_flags = (5 << 12) | 0x10  # doff=5, ACK only
    tcp_hdr = struct.pack('!HHIIHHHH',
        DPORT, SPORT, 1, ack_seq, doff_flags, 502, 0, 0)
    return ip_hdr + tcp_hdr

# 8 warm-up egress packets (pkt_count must reach STALL_MIN_PACKETS=8
# before eligibility is even checked), each advancing seq by 100 bytes
# of payload, all deep inside the >2^31 region.
for i in range(8):
    open(f'{workdir}/egress_warmup_{i}.bin', 'wb').write(
        build_egress(BASE_SEQ + i * 100, 100))

# One ingress ACK, also >2^31, acknowledging some but not all of the
# warm-up data - this is the update that silently failed before the
# fix (diff = (s32)(ack - 0) was negative, so highest_ack never moved
# off 0).
open(f'{workdir}/ingress_ack.bin', 'wb').write(
    build_ingress(BASE_SEQ + 400))

# Final egress packet, same flow, no new data beyond what's already
# been sent - its only purpose is to be observed after a real-time
# gap (the calling script sleeps before injecting it) so the stall
# check actually runs against a large delta. seq equals the current
# end_seq (last warm-up packet's seq+payload = BASE_SEQ + 700 + 100 =
# BASE_SEQ + 800), payload_len=0 so it doesn't move end_seq further.
open(f'{workdir}/egress_final.bin', 'wb').write(
    build_egress(BASE_SEQ + 800, 0))

print(f"wrote packets to {workdir}")
PYEOF

# awk, not `head -1 | cut`: under set -o pipefail, head closing the
# pipe after one line sends bpftool a SIGPIPE, which pipefail then
# treats as a failed command and aborts the whole script via set -e -
# even though bpftool "worked." awk reads its input to EOF, so no
# early close, no SIGPIPE, no spurious abort.
PROG_ID=$(bpftool prog show name egress_monitor 2>/dev/null | awk -F: 'NR==1{print $1}')
INGRESS_PROG_ID=$(bpftool prog show name ingress_monitor 2>/dev/null | awk -F: 'NR==1{print $1}')
if [ -z "$PROG_ID" ] || [ -z "$INGRESS_PROG_ID" ]; then
    echo "egress_monitor/ingress_monitor not loaded - attach glitch-tool/dawn-agent/glitch-exporter to an interface first" >&2
    exit 1
fi
echo "egress_monitor id=$PROG_ID  ingress_monitor id=$INGRESS_PROG_ID"

echo "--- injecting 8 warm-up egress packets (seq starting at $(( 3000000000 )), all > 2^31) ---"
for i in 0 1 2 3 4 5 6 7; do
    bpftool prog run id "$PROG_ID" data_in "$WORKDIR/egress_warmup_$i.bin" data_out "$WORKDIR/out.bin" >/dev/null
done

echo "--- injecting one ingress ACK (also > 2^31) ---"
bpftool prog run id "$INGRESS_PROG_ID" data_in "$WORKDIR/ingress_ack.bin" data_out "$WORKDIR/out.bin" >/dev/null

echo "--- sleeping 1s so the next egress packet sees a real gap ---"
sleep 1

echo "--- injecting final egress packet - should trigger a stall START if highest_ack tracking survived the >2^31 region ---"
bpftool prog run id "$PROG_ID" data_in "$WORKDIR/egress_final.bin" data_out "$WORKDIR/out.bin" >/dev/null

echo
echo "Check the attached glitch-tool's output now for:"
echo "  🟡 [KERNEL] Stall START ... | TCP 10.9.9.1:51000 -> 10.9.9.2:5999"
echo "Its absence means the >2^31 highest_ack bug has regressed."
