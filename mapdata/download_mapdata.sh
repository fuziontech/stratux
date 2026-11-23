#!/bin/bash
set -euo pipefail

if [ "${EUID}" -ne 0 ]; then
    echo "This script must be run as root (try: sudo $0)." >&2
    exit 1
fi

MAPDATA_DIR="/overlay/robase/opt/stratux/mapdata"
if [ ! -d "${MAPDATA_DIR}" ]; then
    MAPDATA_DIR="/opt/stratux/mapdata"
fi

timesync_started=0
overlay_unlocked=0
cleanup() {
    cd / || true
    if [ "${timesync_started}" -eq 1 ]; then
        systemctl stop systemd-timesyncd 2>/dev/null || true
    fi
    sync
    if [ "${overlay_unlocked}" -eq 1 ]; then
        overlayctl lock || true
    fi
}
trap cleanup EXIT

cd /
overlayctl unlock
overlay_unlocked=1
cd "${MAPDATA_DIR}"

echo "Waiting for time synchronization..."
if systemctl start systemd-timesyncd 2>/dev/null; then
    timesync_started=1
fi

ntp_timeout=60
ntp_elapsed=0
while ! timedatectl show 2>/dev/null | grep -q '^NTPSynchronized=yes$'; do
    if [ "${ntp_elapsed}" -ge "${ntp_timeout}" ]; then
        echo
        echo "Warning: NTP did not synchronize within ${ntp_timeout}s; continuing anyway." >&2
        break
    fi
    echo -n "."
    sleep 1
    ntp_elapsed=$((ntp_elapsed + 1))
done
if [ "${timesync_started}" -eq 1 ]; then
    systemctl stop systemd-timesyncd 2>/dev/null || true
    timesync_started=0
fi

echo
read -p "Download OpenFlightMaps VFR Charts Europe (~700 MiB)? [y/n] " -n 1 -r
if [[ "${REPLY}" =~ ^[Yy]$ ]]; then
    echo
    wget -N "https://abatzill.de/stratux/openflightmaps.mbtiles"
fi

echo
read -p "Download US Sectional VFR Charts (~4.9 GiB)? [y/n] " -n 1 -r
if [[ "${REPLY}" =~ ^[Yy]$ ]]; then
    echo
    wget -N "https://abatzill.de/stratux/vfrsec.mbtiles"
fi

echo
cd /
sync
overlayctl lock
overlay_unlocked=0

systemctl restart stratux
