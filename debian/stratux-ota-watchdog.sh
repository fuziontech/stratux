#!/bin/bash
#
# Stratux post-OTA health-check watchdog and automatic rollback script.
# Executed at boot by stratux-ota-watchdog.service (After=stratux.service).
#
# If no OTA update is pending, this script exits 0 immediately as a no-op.
# When an OTA update is pending, it verifies that stratux.service stays active
# and responds on http://127.0.0.1/getStatus for 3 consecutive checks within
# 45 seconds. On success it commits the update and cleans up the backup; on
# failure it restores the previous snapshot and reboots cleanly.

set -euo pipefail

SCRIPT="$(basename -- "${BASH_SOURCE[0]}")"
STX_LOG="/var/log/stratux.log"
ROLLBACK_LOG="/var/log/stratux-ota-rollback.log"

OTA_REL_DIR="var/lib/stratux"
OTA_PENDING_REL="${OTA_REL_DIR}/ota_pending"
OTA_BACKUP_REL="${OTA_REL_DIR}/ota_backup/previous.tar.gz"
OTA_STATUS_REL="${OTA_REL_DIR}/ota_last_status"

MAX_WAIT_SECONDS=45
POLL_INTERVAL=3
REQUIRED_CONSECUTIVE=3
HEALTH_URL="http://127.0.0.1/getStatus"

wLog() {
	mkdir -p "$(dirname -- "${STX_LOG}")" 2>/dev/null || true
	echo "$(date +"%Y/%m/%d %H:%M:%S")  - ${SCRIPT} - $1" >> "${STX_LOG}"
}

overlay_is_active() {
	local fstype
	fstype="$(findmnt -n -o FSTYPE / 2>/dev/null || true)"
	[ "${fstype}" = "overlay" ]
}

# Fast no-op exit when no OTA update is pending.
if [ ! -f "/${OTA_PENDING_REL}" ] && [ ! -f "/overlay/robase/${OTA_PENDING_REL}" ]; then
	exit 0
fi

wLog "Pending OTA update detected; starting health verification (window=${MAX_WAIT_SECONDS}s, interval=${POLL_INTERVAL}s, required=${REQUIRED_CONSECUTIVE})."

check_http_status() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 2 "${HEALTH_URL}" >/dev/null 2>&1
		return $?
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- --timeout=2 "${HEALTH_URL}" >/dev/null 2>&1
		return $?
	fi
	wLog "WARNING: Neither curl nor wget is installed; falling back to systemd active check only."
	return 0
}

health_check() {
	systemctl is-active --quiet stratux && check_http_status
}

start_ts="$(date +%s)"
consecutive_ok=0
healthy=false

while true; do
	if health_check; then
		consecutive_ok=$((consecutive_ok + 1))
		wLog "OTA health check passed (${consecutive_ok}/${REQUIRED_CONSECUTIVE})."
		if (( consecutive_ok >= REQUIRED_CONSECUTIVE )); then
			healthy=true
			break
		fi
	else
		if (( consecutive_ok > 0 )); then
			wLog "OTA health check failed; resetting consecutive pass count."
		fi
		consecutive_ok=0
	fi

	now_ts="$(date +%s)"
	if (( now_ts - start_ts >= MAX_WAIT_SECONDS )); then
		break
	fi
	sleep "${POLL_INTERVAL}"
done

ts_utc="$(date -u +%FT%TZ)"

if [ "${healthy}" = "true" ]; then
	wLog "OTA update verified healthy; committing update."
	if overlay_is_active; then
		/sbin/overlayctl unlock
		mkdir -p "/overlay/robase/${OTA_REL_DIR}"
		rm -f -- "/overlay/robase/${OTA_PENDING_REL}" "/overlay/robase/${OTA_BACKUP_REL}"
		printf 'ota_last_status="committed %s"\n' "${ts_utc}" > "/overlay/robase/${OTA_STATUS_REL}"
		sync
		/sbin/overlayctl lock
	fi
	mkdir -p "/${OTA_REL_DIR}"
	rm -f -- "/${OTA_PENDING_REL}" "/${OTA_BACKUP_REL}"
	printf 'ota_last_status="committed %s"\n' "${ts_utc}" > "/${OTA_STATUS_REL}"
	wLog "OTA update committed successfully at ${ts_utc}."
	exit 0
fi

# Health verification failed — roll back to the pre-update backup snapshot.
svc_state="$(systemctl is-active stratux 2>/dev/null || true)"
pending_info="$(tr '\n' ' ' < "/${OTA_PENDING_REL}" 2>/dev/null || tr '\n' ' ' < "/overlay/robase/${OTA_PENDING_REL}" 2>/dev/null || echo "unknown")"
reason="stratux failed health checks after ${MAX_WAIT_SECONDS}s (service_state=${svc_state:-inactive}, pending=${pending_info})"

wLog "ERROR: OTA health check failed: ${reason}. Initiating automatic rollback."
mkdir -p "$(dirname -- "${ROLLBACK_LOG}")" 2>/dev/null || true
echo "${ts_utc} ROLLBACK: ${reason}" >> "${ROLLBACK_LOG}"

backup_archive=""
if overlay_is_active && [ -f "/overlay/robase/${OTA_BACKUP_REL}" ]; then
	backup_archive="/overlay/robase/${OTA_BACKUP_REL}"
elif [ -f "/${OTA_BACKUP_REL}" ]; then
	backup_archive="/${OTA_BACKUP_REL}"
fi

target_root="/"
if overlay_is_active; then
	/sbin/overlayctl unlock
	target_root="/overlay/robase"
fi

if [ -n "${backup_archive}" ] && [ -f "${backup_archive}" ]; then
	if tar -xzf "${backup_archive}" -C "${target_root}" >> "${STX_LOG}" 2>&1; then
		wLog "Extracted rollback archive ${backup_archive} into ${target_root}."
	else
		wLog "ERROR: Failed to extract rollback archive ${backup_archive} into ${target_root}."
	fi
else
	wLog "ERROR: Rollback archive not found; unable to restore previous files."
fi

mkdir -p "${target_root}/${OTA_REL_DIR}" "${target_root}/var/log"
echo "${ts_utc} ROLLBACK: ${reason}" >> "${target_root}/var/log/stratux-ota-rollback.log"
printf 'ota_last_status="rolled_back %s"\n' "${ts_utc}" > "${target_root}/${OTA_STATUS_REL}"
rm -f -- "${target_root}/${OTA_PENDING_REL}" "/${OTA_PENDING_REL}"

sync
if overlay_is_active; then
	/sbin/overlayctl lock || true
fi

wLog "Rollback complete. Rebooting into previous Stratux version..."
reboot
exit 0
