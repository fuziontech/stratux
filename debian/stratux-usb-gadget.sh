#!/bin/bash
#
# Stratux USB-C Tethered EFB Networking (USB-NCM ConfigFS Gadget).
#
# On Raspberry Pi 4 / Pi 5 / Zero 2W with a USB Device Controller (UDC),
# exposes a plug-and-play USB-NCM network interface (usb0, 192.168.11.1/24)
# for iPadOS / macOS / Linux EFBs without stealing the EFB's default route
# or DNS (DHCP options 3 and 6 are intentionally omitted).
#
# On hardware without a UDC (e.g. Raspberry Pi 3 or desktop Linux), exits 0
# cleanly so boot is never impeded.

set -euo pipefail
shopt -s nullglob

GADGET_DIR="/sys/kernel/config/usb_gadget/stratux"
IFACE="usb0"
USB_IP="192.168.11.1/24"
DHCP_RANGE="192.168.11.10,192.168.11.50,12h"
DNSMASQ_PID="/run/stratux-usb0-dnsmasq.pid"
DNSMASQ_LEASES="/var/lib/misc/dnsmasq.usb0.leases"

# Deterministic locally-administered unicast MAC addresses (02:53:54:58 = "STX").
HOST_MAC="02:53:54:58:00:01"
DEV_MAC="02:53:54:58:00:02"

stop_dnsmasq() {
	if [ -f "${DNSMASQ_PID}" ]; then
		local pid
		pid="$(cat "${DNSMASQ_PID}" 2>/dev/null || true)"
		if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
			kill "${pid}" 2>/dev/null || true
		fi
		rm -f -- "${DNSMASQ_PID}"
	fi
}

get_serial_number() {
	local serial=""
	if [ -r /proc/device-tree/serial-number ]; then
		serial="$(tr -d '\0\r\n' < /proc/device-tree/serial-number 2>/dev/null || true)"
	fi
	if [ -z "${serial}" ]; then
		serial="stratux0001"
	fi
	printf '%s\n' "${serial}"
}

start_gadget() {
	local udcs=( /sys/class/udc/* )
	if (( ${#udcs[@]} == 0 )); then
		echo "stratux-usb-gadget: no UDC present in /sys/class/udc; skipping USB-NCM setup."
		exit 0
	fi
	local udc
	udc="$(basename -- "${udcs[0]}")"

	modprobe libcomposite 2>/dev/null || true

	if ! mountpoint -q /sys/kernel/config 2>/dev/null; then
		if ! mount -t configfs none /sys/kernel/config 2>/dev/null; then
			echo "stratux-usb-gadget: unable to mount configfs; skipping USB-NCM setup."
			exit 0
		fi
	fi

	if [ ! -d /sys/kernel/config/usb_gadget ]; then
		echo "stratux-usb-gadget: kernel USB gadget configfs not available; skipping."
		exit 0
	fi

	# If an existing gadget is already bound, unbind before reconfiguring.
	if [ -f "${GADGET_DIR}/UDC" ]; then
		local cur_udc
		cur_udc="$(cat "${GADGET_DIR}/UDC" 2>/dev/null || true)"
		if [ -n "${cur_udc}" ]; then
			echo "" > "${GADGET_DIR}/UDC" 2>/dev/null || true
		fi
	fi

	mkdir -p "${GADGET_DIR}"
	echo 0x1d6b > "${GADGET_DIR}/idVendor"   # Linux Foundation
	echo 0x0104 > "${GADGET_DIR}/idProduct"  # Multifunction Composite Gadget
	echo 0x0100 > "${GADGET_DIR}/bcdDevice"
	echo 0x0200 > "${GADGET_DIR}/bcdUSB"
	echo 0xef   > "${GADGET_DIR}/bDeviceClass"
	echo 0x02   > "${GADGET_DIR}/bDeviceSubClass"
	echo 0x01   > "${GADGET_DIR}/bDeviceProtocol"

	mkdir -p "${GADGET_DIR}/strings/0x409"
	get_serial_number > "${GADGET_DIR}/strings/0x409/serialnumber"
	echo "Stratux" > "${GADGET_DIR}/strings/0x409/manufacturer"
	echo "Stratux EFB Network (USB-NCM)" > "${GADGET_DIR}/strings/0x409/product"

	mkdir -p "${GADGET_DIR}/configs/c.1/strings/0x409"
	echo "USB-NCM Network" > "${GADGET_DIR}/configs/c.1/strings/0x409/configuration"
	echo 250 > "${GADGET_DIR}/configs/c.1/MaxPower"

	mkdir -p "${GADGET_DIR}/functions/ncm.usb0"
	echo "${HOST_MAC}" > "${GADGET_DIR}/functions/ncm.usb0/host_addr"
	echo "${DEV_MAC}"  > "${GADGET_DIR}/functions/ncm.usb0/dev_addr"

	if [ ! -L "${GADGET_DIR}/configs/c.1/ncm.usb0" ]; then
		ln -s "${GADGET_DIR}/functions/ncm.usb0" "${GADGET_DIR}/configs/c.1/ncm.usb0"
	fi

	echo "${udc}" > "${GADGET_DIR}/UDC"

	# Wait briefly for the kernel to expose the usb0 network interface.
	local i
	for i in 1 2 3 4 5 6 7 8 9 10; do
		if ip link show "${IFACE}" >/dev/null 2>&1; then
			break
		fi
		sleep 0.5
	done

	if ! ip link show "${IFACE}" >/dev/null 2>&1; then
		echo "stratux-usb-gadget: interface ${IFACE} did not appear after binding ${udc}." >&2
		exit 0
	fi

	ip addr flush dev "${IFACE}" 2>/dev/null || true
	ip addr add "${USB_IP}" dev "${IFACE}"
	ip link set dev "${IFACE}" up

	stop_dnsmasq
	if command -v dnsmasq >/dev/null 2>&1; then
		mkdir -p "$(dirname -- "${DNSMASQ_LEASES}")"
		# Omit DHCP options 3 (default router) and 6 (DNS) so iPadOS/EFB
		# retains cellular/Starlink internet while receiving GDL90 over USB-C.
		dnsmasq \
			--conf-file=/dev/null \
			--interface="${IFACE}" \
			--bind-interfaces \
			--except-interface=lo \
			--port=0 \
			--dhcp-range="${DHCP_RANGE}" \
			--dhcp-option=3 \
			--dhcp-option=6 \
			--dhcp-leasefile="${DNSMASQ_LEASES}" \
			--pid-file="${DNSMASQ_PID}"
	fi
}

stop_gadget() {
	stop_dnsmasq

	if ip link show "${IFACE}" >/dev/null 2>&1; then
		ip link set dev "${IFACE}" down 2>/dev/null || true
		ip addr flush dev "${IFACE}" 2>/dev/null || true
	fi

	if [ -d "${GADGET_DIR}" ]; then
		if [ -f "${GADGET_DIR}/UDC" ]; then
			echo "" > "${GADGET_DIR}/UDC" 2>/dev/null || true
		fi
		rm -f -- "${GADGET_DIR}/configs/c.1/ncm.usb0" 2>/dev/null || true
		rmdir "${GADGET_DIR}/configs/c.1/strings/0x409" 2>/dev/null || true
		rmdir "${GADGET_DIR}/configs/c.1" 2>/dev/null || true
		rmdir "${GADGET_DIR}/functions/ncm.usb0" 2>/dev/null || true
		rmdir "${GADGET_DIR}/strings/0x409" 2>/dev/null || true
		rmdir "${GADGET_DIR}" 2>/dev/null || true
	fi
}

case "${1:-start}" in
	start)
		start_gadget
		;;
	stop)
		stop_gadget
		;;
	restart)
		stop_gadget
		start_gadget
		;;
	*)
		echo "Usage: $0 {start|stop|restart}" >&2
		exit 1
		;;
esac

exit 0
