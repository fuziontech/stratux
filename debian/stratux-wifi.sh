#!/bin/bash
#####
#####	Wifi/AP control file
#####
#####	Description: All the scripting related to the AP and wireless functions for Stratux should be placed in this file
#####	This file is called when wlan0/ap0 is started i.e. "ifup wlan0" / "ifup ap0"
#####	This script is called from /etc/network/interfaces by the line "post-up /opt/stratux/bin/stratux-wifi.sh"
#####


# Logging Function
SCRIPT="$(basename -- "${BASH_SOURCE[0]}")"
STX_LOG="/var/log/stratux.log"
wLog() {
	echo "$(date +"%Y/%m/%d %H:%M:%S")  - ${SCRIPT} - $1" >> "${STX_LOG}"
}
wLog "Running Stratux WiFI Script."

interface="${1:-}" # for dhcp and wpa_supplicant
mode="${2:-0}"     # 0=ap, 1=wifi-direct, 2=ap+client
pin="${3:-}"       # wifi-direct pin

if [ "${interface}" = "0" ] || [ "${interface}" = "1" ] || [ "${interface}" = "2" ]; then
	# compatibility to old /etc/network/interfaces before eu027
	echo "COMPAT MODE"
	interface="wlan0"
	mode="${1:-0}"
	pin="${2:-}"
fi

if [ -z "${interface}" ]; then
	interface="wlan0"
fi

echo "interface=${interface},mode=${mode}"

WPS_PIN_PIDFILE="/run/stratux_wps_pin.pid"

terminate() {
	# Given a PID file and optional expected comm regex (default: ^wpa_supplicant$),
	# validate the PID and process name before terminating.
	local pidfile="$1"
	local expected_comm="${2:-^wpa_supplicant$}"

	if [[ -f "${pidfile}" ]]; then
		local pid
		pid="$(tr -d '[:space:]' < "${pidfile}")"
		rm -f -- "${pidfile}"

		if [[ ! "${pid}" =~ ^[0-9]+$ ]]; then
			wLog "Ignoring non-numeric PID in ${pidfile}: ${pid}"
			return
		fi

		if [[ ! -r "/proc/${pid}/comm" ]]; then
			return
		fi

		local comm
		comm="$(cat "/proc/${pid}/comm" 2>/dev/null || true)"
		if [[ ! "${comm}" =~ ${expected_comm} ]]; then
			wLog "Skipping PID ${pid} from ${pidfile}: comm '${comm}' does not match '${expected_comm}'"
			return
		fi

		echo "killing ${pid} (${comm})"
		kill "${pid}" 2>/dev/null || true
		local i
		for i in $(seq 10); do
			# If process exits successfully, we are done
			echo "checking..."
			if ! ps -p "${pid}" >/dev/null 2>&1; then
				echo "terminated ${pid}"
				return
			fi
			sleep 0.5
		done
		# Didn't exit in 5 secs.. kill it
		echo "could not kill ${pid}. Hard kill"
		kill -9 "${pid}" 2>/dev/null || true
		sleep 1
	fi
}

prepare-start() {
	# Preliminaries. Kill off old services.
	wLog "Killing wpa_supplicant AP services"
	terminate "${WPS_PIN_PIDFILE}" "^(bash|sh|stratux-wifi\.sh)$"
	terminate /run/wpa_supplicant_ap.pid "^wpa_supplicant$"
	terminate /run/wpa_supplicant_p2p.pid "^wpa_supplicant$"

	wLog "Stopping DHCP services"
	/bin/systemctl stop dnsmasq 2>/dev/null || true
	/usr/bin/killall dnsmasq 2>/dev/null || true
}

ap-start() {
	echo "Starting AP mode on ${interface}"

	/sbin/wpa_supplicant -P/run/wpa_supplicant_ap.pid -B -i "${interface}" -c /etc/wpa_supplicant/wpa_supplicant_ap.conf
	sleep 2

	wLog "Restarting DHCP services"
	dnsmasq -u dnsmasq --conf-dir=/etc/dnsmasq.d -i "${interface}"
}

wifi-direct-start() {
	echo "Starting wifi direct mode on ${interface}"

	local p2p_iface="p2p-${interface}-0"

	/sbin/wpa_supplicant -P/run/wpa_supplicant_p2p.pid -B -i "${interface}" -c /etc/wpa_supplicant/wpa_supplicant.conf

	# Wait briefly for wpa_supplicant control socket to become ready
	local retries=0
	while [ ! -S "/var/run/wpa_supplicant/${interface}" ] && [ "${retries}" -lt 50 ]; do
		sleep 0.2
		retries=$((retries + 1))
	done

	wpa_cli -i "${interface}" p2p_group_add persistent=0 freq=2

	# Wait briefly for the P2P group interface to appear
	retries=0
	while [ ! -d "/sys/class/net/${p2p_iface}" ] && [ "${retries}" -lt 50 ]; do
		sleep 0.2
		retries=$((retries + 1))
	done

	(
		while wpa_cli -i "${p2p_iface}" wps_pin any "${pin}" >/dev/null 2>&1; do
			sleep 1
		done
	) &
	local wps_pid=$!
	echo "${wps_pid}" > "${WPS_PIN_PIDFILE}"
	disown "${wps_pid}" 2>/dev/null || true

	ifup "${p2p_iface}"

	dnsmasq -u dnsmasq --conf-dir=/etc/dnsmasq.d -i "${p2p_iface}"
}

prepare-start
if [ "${mode}" = "1" ]; then
	wifi-direct-start
else
	ap-start
fi
