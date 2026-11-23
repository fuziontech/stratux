#!/bin/bash
# Run this script on your stratux and pass it the IP address of the WiFi client to use for internet connection
# Usage: ./inet_via_wifi_rpi.sh 192.168.10.x
set -euo pipefail

if [ "$#" -lt 1 ] || [ -z "$1" ]; then
    echo "Usage: $0 <gateway-ip>" >&2
    exit 1
fi

gw="$1"
ip route replace default via "${gw}"
echo "nameserver 8.8.8.8" > /etc/resolv.conf
