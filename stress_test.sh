#!/bin/bash
# DAWN6G Stress Test: Validating 5QI Priority

TARGET="dawn6g.duckdns.org"
DURATION=60

echo "🚀 Starting Baseline TWAMP (Clean Link)..."
twampy controller $TARGET --count 10 --dscp ef > baseline.txt

echo "🔥 INJECTING CONGESTION: Starting 50Mbps UDP Flood (Best Effort)..."
# -u (UDP), -b (Bandwidth), -t (Time)
# We do NOT use --dscp here, so it stays as Best Effort (5QI 9)
iperf3 -c $TARGET -u -b 50M -t $DURATION --bidir &

sleep 5
echo "📡 TESTING PRIORITY: Running TWAMP on EF Slice during congestion..."
twampy controller $TARGET --count 20 --dscp ef --interval 100

echo "🛑 Cleaning up..."
pkill iperf3
echo "Done. Compare the Jitter in the EF TWAMP output to the Iperf3 packet loss."
