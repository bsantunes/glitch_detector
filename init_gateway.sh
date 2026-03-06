#!/bin/bash
# DAWN6G Hardware & Kernel Initialization

INTERFACE="wwan0"

# 1. Load Kernel Modules
modprobe cls_bpf
modprobe sch_ingress
modprobe sch_htb

# 2. Setup SIM8202G-M2 QoS Mapping
# cid 2 = EF/Critical traffic, mapped to DSCP 46 (TOS 184)
mmcli -m 0 --command='AT+CGDSCONT=2,1'
mmcli -m 0 --command='AT+CGTFT=2,1,0,"",0,"","",184,255'
mmcli -m 0 --command='AT+CGACT=1,2'

# 3. Setup TC HTB Queues (Priority Lane 1:10, Passenger Lane 1:20)
tc qdisc del dev $INTERFACE root 2>/dev/null
tc qdisc add dev $INTERFACE root handle 1: htb default 20
tc class add dev $INTERFACE parent 1: classid 1:1 htb rate 100mbit
tc class add dev $INTERFACE parent 1:1 classid 1:10 htb rate 100mbit ceil 100mbit
tc class add dev $INTERFACE parent 1:1 classid 1:20 htb rate 100mbit ceil 100mbit
# Filter DSCP EF into the Priority Lane
tc filter add dev $INTERFACE protocol ip parent 1:0 prio 1 u32 match ip tos 0xb8 0xff flowid 1:10

echo "✅ Gateway Hardware Initialized"
