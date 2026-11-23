#!/bin/bash
# Run this script on your desktop PC which is connected to the Stratux via WiFi and to the internet via cable
# Usage: ./inet_via_wifi_pc.sh enp0s1
# where enp0s1 is the interface name of your outgoing internet connection
set -euo pipefail

if [ "$#" -lt 1 ] || [ -z "$1" ]; then
    echo "Usage: $0 <outgoing-interface>" >&2
    exit 1
fi

iface="$1"
sysctl -w net.ipv4.ip_forward=1
iptables -t nat -C POSTROUTING -o "${iface}" -j MASQUERADE 2>/dev/null \
    || iptables -t nat -A POSTROUTING -o "${iface}" -j MASQUERADE
