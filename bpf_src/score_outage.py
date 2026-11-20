#!/usr/bin/env python3
"""Score a glitch-tool log against a real outage's own wall-clock window.

Pass criterion (per explicit review correction — "at least one matching
stall" is not sufficient, since it would also accept an outage that got
spuriously split into two episodes if either fragment alone happened to
be long enough):

  exactly one stall episode OVERLAPS the outage window
    (episode.start <= outage_end AND episode.end >= outage_start)
  AND that episode's duration > the outage length.

Any other episode (fully outside the window) is reported but does not
affect the verdict — a real link can produce its own short, unrelated
stall from ordinary jitter (confirmed live, see CLAUDE.md/test3's
header). Two or more OVERLAPPING episodes is always a FAIL: that is
exactly the "outage wrongly split into two" case this script exists to
catch, which a bare duration check cannot see.

Requires glitch-tool's event lines to start with an RFC3339Nano UTC
timestamp (added specifically to make this script possible — the lines
previously carried only relative gap/duration values, not the absolute
time needed to compare against the outage's own window).

Usage:
  score_outage.py <log> <dst_port_suffix> <outage_start_epoch> <outage_end_epoch> <outage_seconds>

Exit 0 and prints "RESULT: PASS" on pass; exit 1 and "RESULT: FAIL: <reason>" otherwise.
"""
import re
import sys
from datetime import datetime, timezone

LINE_RE = re.compile(r"^(?P<ts>\S+) .*\[(?P<tag>\w+)\] Stall (?P<kind>START|END):.*\| (?P<proto>\S+) (?P<src>\S+) -> (?P<dst>\S+)$")
DURATION_RE = re.compile(r"duration ([0-9.]+)ms")


def parse_ts(s):
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    return datetime.fromisoformat(s).timestamp()


def main():
    if len(sys.argv) != 6:
        print("usage: score_outage.py <log> <dst_port_suffix> <outage_start> <outage_end> <outage_seconds>", file=sys.stderr)
        sys.exit(2)
    log_path, port_suffix, outage_start, outage_end, outage_seconds = sys.argv[1:]
    outage_start = float(outage_start)
    outage_end = float(outage_end)
    outage_ms = float(outage_seconds) * 1000.0

    # Keyed per flow (proto, src ip:port, dst ip:port), not a single
    # scalar — a run with more than one concurrent flow to the same
    # dst port (e.g. iperf3 -P N) interleaves each flow's own
    # START/END lines in the log, and a single open_start variable
    # confuses flow A's START with flow B's END. Confirmed live: a -P 4
    # run produced spurious "START seen while another was already
    # open" warnings and false episode-split FAILs purely from this
    # scoring bug, not a glitch/core defect.
    episodes = []
    open_by_flow = {}
    unmatched_ends = 0
    with open(log_path, "r", errors="replace") as f:
        for line in f:
            line = line.rstrip("\n")
            if not line.endswith(port_suffix):
                continue
            m = LINE_RE.match(line)
            if not m:
                continue
            ts = parse_ts(m.group("ts"))
            flow_key = (m.group("proto"), m.group("src"), m.group("dst"))
            if m.group("kind") == "START":
                if flow_key in open_by_flow:
                    print(f"WARNING: Stall START seen for {flow_key} while another was already open (at {m.group('ts')}) — unexpected, treating the earlier one as unmatched", file=sys.stderr)
                open_by_flow[flow_key] = ts
            else:  # END
                dm = DURATION_RE.search(line)
                duration_ms = float(dm.group(1)) if dm else None
                if flow_key not in open_by_flow:
                    unmatched_ends += 1
                    print(f"WARNING: Stall END with no preceding START for {flow_key} (at {m.group('ts')})", file=sys.stderr)
                    continue
                episodes.append((open_by_flow.pop(flow_key), ts, duration_ms))

    for flow_key, start in open_by_flow.items():
        print(f"NOTE: an episode started but never closed by end of log (flow={flow_key} start={start})", file=sys.stderr)

    overlapping = []
    for start, end, duration_ms in episodes:
        overlaps = start <= outage_end and end >= outage_start
        tag = "OVERLAPS" if overlaps else "outside"
        print(f"episode: start={start:.3f} end={end:.3f} duration={duration_ms}ms [{tag}]")
        if overlaps:
            overlapping.append((start, end, duration_ms))

    if len(overlapping) == 0:
        print(f"RESULT: FAIL: no episode overlaps the outage window ({outage_start:.3f}..{outage_end:.3f})")
        sys.exit(1)
    if len(overlapping) > 1:
        print(f"RESULT: FAIL: {len(overlapping)} episodes overlap the outage window — outage was split into multiple episodes instead of one")
        sys.exit(1)

    start, end, duration_ms = overlapping[0]
    if duration_ms is None or duration_ms <= outage_ms:
        print(f"RESULT: FAIL: the one overlapping episode's duration ({duration_ms}ms) does not exceed the outage length ({outage_ms}ms)")
        sys.exit(1)

    print(f"RESULT: PASS: one episode overlaps the outage window, duration {duration_ms}ms > {outage_ms}ms outage")
    sys.exit(0)


if __name__ == "__main__":
    main()
