#!/bin/bash
#
# Stratux pre-start OTA updater and first-boot configuration script.
# Executed by stratux-update.service (Type=oneshot, Before=stratux.service).
#
# Optional OTA cryptographic signature verification:
#   - If /opt/stratux/cfg/ota-pubkey.pub exists (minisign public key), every
#     staged update file MUST have a valid detached signature (<file>.minisig
#     or <file>.sig) verified by `minisign`.
#   - If /opt/stratux/cfg/ota-pubkey.asc exists (GPG public key), every staged
#     update file MUST have a valid detached signature (<file>.sig or
#     <file>.asc) verified by `gpg`/`gpgv`.
#   - If neither public key file exists, signature verification is skipped
#     for backward compatibility with unsigned local builds.

shopt -s nullglob

SCRIPT="$(basename -- "${BASH_SOURCE[0]}")"
STX_LOG="/var/log/stratux.log"
wLog() {
	echo "$(date +"%Y/%m/%d %H:%M:%S")  - ${SCRIPT} - $1" >> "${STX_LOG}"
}
wLog "Running Stratux Updater Script."

TEMP_DIRECTORY="/boot/firmware/StratuxUpdates"
MINISIGN_PUBKEY="/opt/stratux/cfg/ota-pubkey.pub"
GPG_PUBKEY="/opt/stratux/cfg/ota-pubkey.asc"
OTA_STATE_DIR="/var/lib/stratux"
OTA_BACKUP_DIR="${OTA_STATE_DIR}/ota_backup"
OTA_BACKUP_ARCHIVE="${OTA_BACKUP_DIR}/previous.tar.gz"
OTA_PENDING_FILE="${OTA_STATE_DIR}/ota_pending"
OTA_STATUS_FILE="${OTA_STATE_DIR}/ota_last_status"
OTA_ROLLBACK_LOG="/var/log/stratux-ota-rollback.log"

######################
# script based update
SCRIPT_MASK="update*stratux*v*.sh"

######################
# package based update
PACKAGE_MASK="stratux*.deb"

# Detect whether the overlay filesystem is currently active.
# /overlay/robase is always present (used by overlayctl for management), so we
# check the root mount type instead of directory existence.
overlay_is_active() {
	[ "$(findmnt -n -o FSTYPE /)" = "overlay" ]
}

do_reboot() {
	wLog "$1"
	sync
	reboot
	exit 0
}

# Return the newest regular file from the given arguments.
newest_matching_file() {
	local newest=""
	local f
	for f in "$@"; do
		[ -f "$f" ] || continue
		if [ -z "$newest" ] || [ "$f" -nt "$newest" ]; then
			newest="$f"
		fi
	done
	if [ -n "$newest" ]; then
		printf '%s\n' "$newest"
	fi
}

# Reject filenames containing unexpected characters or path traversal.
validate_filename() {
	local base
	base="$(basename -- "$1")"
	if [[ ! "$base" =~ ^[A-Za-z0-9._-]+$ ]]; then
		wLog "ERROR: Rejecting update file with invalid filename: ${base}"
		return 1
	fi
	return 0
}

# Verify optional detached cryptographic signature when an OTA public key is configured.
verify_update_signature() {
	local file="$1"
	local sig_file=""

	if [ -f "${MINISIGN_PUBKEY}" ]; then
		if ! command -v minisign >/dev/null 2>&1; then
			wLog "ERROR: ${MINISIGN_PUBKEY} is configured, but minisign is not installed."
			return 1
		fi
		if [ -f "${file}.minisig" ]; then
			sig_file="${file}.minisig"
		elif [ -f "${file}.sig" ]; then
			sig_file="${file}.sig"
		else
			wLog "ERROR: Missing minisign signature (.minisig or .sig) for ${file}"
			return 1
		fi
		if ! minisign -Vm "${file}" -p "${MINISIGN_PUBKEY}" -x "${sig_file}" >> "${STX_LOG}" 2>&1; then
			wLog "ERROR: Minisign signature verification failed for ${file}"
			return 1
		fi
		wLog "Verified minisign signature for ${file}"
	fi

	if [ -f "${GPG_PUBKEY}" ]; then
		if [ -f "${file}.sig" ]; then
			sig_file="${file}.sig"
		elif [ -f "${file}.asc" ]; then
			sig_file="${file}.asc"
		else
			wLog "ERROR: Missing GPG signature (.sig or .asc) for ${file}"
			return 1
		fi
		if command -v gpg >/dev/null 2>&1; then
			local tmp_gnupg
			tmp_gnupg="$(mktemp -d)"
			chmod 700 "${tmp_gnupg}"
			if gpg --batch --homedir "${tmp_gnupg}" --import "${GPG_PUBKEY}" >> "${STX_LOG}" 2>&1 \
				&& gpg --batch --homedir "${tmp_gnupg}" --verify "${sig_file}" "${file}" >> "${STX_LOG}" 2>&1; then
				rm -rf "${tmp_gnupg}"
				wLog "Verified GPG signature for ${file}"
			else
				rm -rf "${tmp_gnupg}"
				wLog "ERROR: GPG signature verification failed for ${file}"
				return 1
			fi
		elif command -v gpgv >/dev/null 2>&1; then
			if ! gpgv --keyring "${GPG_PUBKEY}" "${sig_file}" "${file}" >> "${STX_LOG}" 2>&1; then
				wLog "ERROR: gpgv signature verification failed for ${file}"
				return 1
			fi
			wLog "Verified GPG signature for ${file}"
		else
			wLog "ERROR: ${GPG_PUBKEY} is configured, but neither gpg nor gpgv is installed."
			return 1
		fi
	fi

	return 0
}

# Copy any detached signature sidecars alongside a staged update file.
copy_sig_sidecars() {
	local src="$1"
	local dest_dir="$2"
	local ext
	for ext in .sig .minisig .asc; do
		if [ -f "${src}${ext}" ]; then
			cp -- "${src}${ext}" "${dest_dir}/" || return 1
		fi
	done
	return 0
}

remove_sig_sidecars() {
	local base_path="$1"
	rm -f -- "${base_path}.sig" "${base_path}.minisig" "${base_path}.asc"
}

# Snapshot current Stratux installation before applying a .deb OTA update so
# failed installs or unhealthy boots can automatically roll back.
create_ota_backup() {
	mkdir -p "${OTA_BACKUP_DIR}"
	if command -v dpkg-query >/dev/null 2>&1; then
		dpkg-query -W -f='${Version}\n' stratux > "${OTA_BACKUP_DIR}/previous_version.txt" 2>/dev/null \
			|| echo "unknown" > "${OTA_BACKUP_DIR}/previous_version.txt"
	else
		echo "unknown" > "${OTA_BACKUP_DIR}/previous_version.txt"
	fi

	local backup_paths=()
	if [ -d /opt/stratux ]; then
		backup_paths+=( "opt/stratux" )
	fi
	local svc
	for svc in /lib/systemd/system/stratux*.service; do
		if [ -e "${svc}" ]; then
			backup_paths+=( "${svc#/}" )
		fi
	done

	if (( ${#backup_paths[@]} == 0 )); then
		wLog "WARNING: No existing Stratux files found to snapshot for OTA rollback."
		return 1
	fi

	rm -f -- "${OTA_BACKUP_ARCHIVE}"
	if tar -czf "${OTA_BACKUP_ARCHIVE}" -C / "${backup_paths[@]}" >> "${STX_LOG}" 2>&1; then
		wLog "Created OTA rollback snapshot at ${OTA_BACKUP_ARCHIVE}"
		return 0
	fi

	wLog "WARNING: Failed to create OTA rollback snapshot at ${OTA_BACKUP_ARCHIVE}"
	rm -f -- "${OTA_BACKUP_ARCHIVE}"
	return 1
}

restore_ota_backup() {
	local reason="$1"
	local ts
	ts="$(date -u +%FT%TZ)"
	mkdir -p "$(dirname -- "${OTA_ROLLBACK_LOG}")" "${OTA_STATE_DIR}"
	echo "${ts} IMMEDIATE_ROLLBACK: ${reason}" >> "${OTA_ROLLBACK_LOG}"
	if [ -f "${OTA_BACKUP_ARCHIVE}" ]; then
		if tar -xzf "${OTA_BACKUP_ARCHIVE}" -C / >> "${STX_LOG}" 2>&1; then
			wLog "Restored previous Stratux installation from ${OTA_BACKUP_ARCHIVE}."
			systemctl daemon-reload 2>/dev/null || true
		else
			wLog "ERROR: Failed to extract rollback archive ${OTA_BACKUP_ARCHIVE}."
		fi
	else
		wLog "WARNING: No OTA backup archive found at ${OTA_BACKUP_ARCHIVE}; unable to restore files."
	fi
	rm -f -- "${OTA_PENDING_FILE}"
	printf 'ota_last_status="rolled_back %s"\n' "${ts}" > "${OTA_STATUS_FILE}"
}

###############
# Stage 1 (overlay active): script in SD card download location.
# Copy it directly to the lower ext4 layer so it survives the reboot.
temp_script_files=( "${TEMP_DIRECTORY}"/${SCRIPT_MASK} )
if (( ${#temp_script_files[@]} > 0 )); then
	TEMP_SCRIPT_FILE="$(newest_matching_file "${temp_script_files[@]}")"
	if [ -n "${TEMP_SCRIPT_FILE}" ]; then
		wLog "Found update script ${TEMP_SCRIPT_FILE}"
		if ! validate_filename "${TEMP_SCRIPT_FILE}" || ! verify_update_signature "${TEMP_SCRIPT_FILE}"; then
			wLog "ERROR: Update script validation failed for ${TEMP_SCRIPT_FILE}. Update aborted."
		elif overlay_is_active; then
			wLog "Overlay active — staging script to ext4 lower layer and disabling overlay..."
			base_name="$(basename -- "${TEMP_SCRIPT_FILE}")"
			if /sbin/overlayctl unlock \
				&& cp -- "${TEMP_SCRIPT_FILE}" "/overlay/robase/root/${base_name}" \
				&& copy_sig_sidecars "${TEMP_SCRIPT_FILE}" "/overlay/robase/root"; then
				chmod a+x "/overlay/robase/root/${base_name}"
				rm -f -- "${TEMP_SCRIPT_FILE}"
				remove_sig_sidecars "${TEMP_SCRIPT_FILE}"
				/sbin/overlayctl disable
				do_reboot "Script staged. Rebooting to apply on bare ext4..."
			else
				wLog "ERROR: Failed to stage script to ext4 lower layer. Update aborted."
				/sbin/overlayctl lock
			fi
		else
			# Overlay already inactive — copy to /root/ for next section to pick up
			base_name="$(basename -- "${TEMP_SCRIPT_FILE}")"
			if cp -- "${TEMP_SCRIPT_FILE}" "/root/${base_name}" \
				&& copy_sig_sidecars "${TEMP_SCRIPT_FILE}" "/root"; then
				chmod a+x "/root/${base_name}"
				rm -f -- "${TEMP_SCRIPT_FILE}"
				remove_sig_sidecars "${TEMP_SCRIPT_FILE}"
			else
				wLog "ERROR: Failed to copy update script to /root/. Update aborted."
			fi
		fi
	fi
fi

# Stage 2 (overlay inactive): execute the update script from /root/
update_script_files=( /root/${SCRIPT_MASK} )
if (( ${#update_script_files[@]} > 0 )); then
	UPDATE_SCRIPT_FILE="$(newest_matching_file "${update_script_files[@]}")"
	if [ -n "${UPDATE_SCRIPT_FILE}" ]; then
		if ! validate_filename "${UPDATE_SCRIPT_FILE}" || ! verify_update_signature "${UPDATE_SCRIPT_FILE}"; then
			wLog "ERROR: Rejecting staged update script ${UPDATE_SCRIPT_FILE}."
			mv -- "${UPDATE_SCRIPT_FILE}" "${UPDATE_SCRIPT_FILE}.rejected"
			remove_sig_sidecars "${UPDATE_SCRIPT_FILE}"
			rm -f /overlay/disable
		else
			wLog "Executing update script ${UPDATE_SCRIPT_FILE}..."
			base_name="$(basename -- "${UPDATE_SCRIPT_FILE}")"
			UPDATE_TEMP_SCRIPT="/tmp/${base_name}"
			mv -- "${UPDATE_SCRIPT_FILE}" "${UPDATE_TEMP_SCRIPT}"
			remove_sig_sidecars "${UPDATE_SCRIPT_FILE}"
			# Re-enable overlay for next boot before running the update script.
			rm -f /overlay/disable
			if bash "${UPDATE_TEMP_SCRIPT}" >> "${STX_LOG}" 2>&1; then
				wLog "Update script completed successfully."
				rm -f -- "${UPDATE_TEMP_SCRIPT}"
			else
				wLog "ERROR: Update script ${UPDATE_SCRIPT_FILE} failed (preserving as ${UPDATE_SCRIPT_FILE}.failed)."
				mv -- "${UPDATE_TEMP_SCRIPT}" "${UPDATE_SCRIPT_FILE}.failed" || true
			fi
			do_reboot "Finished update script. Rebooting..."
		fi
	fi
fi

##############
# Stage 1 (overlay active): deb in SD card download location.
# Copy it directly to the lower ext4 layer so it survives the reboot,
# then disable overlay so the next boot runs dpkg on bare ext4.
temp_package_files=( "${TEMP_DIRECTORY}"/${PACKAGE_MASK} )
if (( ${#temp_package_files[@]} > 0 )); then
	TEMP_PACKAGE_FILE="$(newest_matching_file "${temp_package_files[@]}")"
	if [ -n "${TEMP_PACKAGE_FILE}" ]; then
		wLog "Found update package ${TEMP_PACKAGE_FILE}"
		if ! validate_filename "${TEMP_PACKAGE_FILE}" || ! verify_update_signature "${TEMP_PACKAGE_FILE}"; then
			wLog "ERROR: Update package validation failed for ${TEMP_PACKAGE_FILE}. Update aborted."
		elif overlay_is_active; then
			wLog "Overlay active — staging package to ext4 lower layer and disabling overlay..."
			base_name="$(basename -- "${TEMP_PACKAGE_FILE}")"
			if /sbin/overlayctl unlock \
				&& cp -- "${TEMP_PACKAGE_FILE}" "/overlay/robase/root/${base_name}" \
				&& copy_sig_sidecars "${TEMP_PACKAGE_FILE}" "/overlay/robase/root"; then
				rm -f -- "${TEMP_PACKAGE_FILE}"
				remove_sig_sidecars "${TEMP_PACKAGE_FILE}"
				/sbin/overlayctl disable
				do_reboot "Package staged. Rebooting to install on bare ext4..."
			else
				wLog "ERROR: Failed to stage package to ext4 lower layer. Update aborted."
				/sbin/overlayctl lock
			fi
		else
			# Overlay already inactive — copy to /root/ for next section to pick up
			base_name="$(basename -- "${TEMP_PACKAGE_FILE}")"
			if cp -- "${TEMP_PACKAGE_FILE}" "/root/${base_name}" \
				&& copy_sig_sidecars "${TEMP_PACKAGE_FILE}" "/root"; then
				rm -f -- "${TEMP_PACKAGE_FILE}"
				remove_sig_sidecars "${TEMP_PACKAGE_FILE}"
			else
				wLog "ERROR: Failed to copy update package to /root/. Update aborted."
			fi
		fi
	fi
fi

# Stage 2 (overlay inactive): install the deb from /root/ and re-enable overlay
update_package_files=( /root/${PACKAGE_MASK} )
if (( ${#update_package_files[@]} > 0 )); then
	UPDATE_PACKAGE_FILE="$(newest_matching_file "${update_package_files[@]}")"
	if [ -n "${UPDATE_PACKAGE_FILE}" ]; then
		if ! validate_filename "${UPDATE_PACKAGE_FILE}" || ! verify_update_signature "${UPDATE_PACKAGE_FILE}"; then
			wLog "ERROR: Rejecting staged update package ${UPDATE_PACKAGE_FILE}."
			mv -- "${UPDATE_PACKAGE_FILE}" "${UPDATE_PACKAGE_FILE}.rejected"
			remove_sig_sidecars "${UPDATE_PACKAGE_FILE}"
			rm -f /overlay/disable
		else
			wLog "Installing update package ${UPDATE_PACKAGE_FILE}..."
			base_name="$(basename -- "${UPDATE_PACKAGE_FILE}")"
			UPDATE_TEMP_FILE="/tmp/${base_name}"
			mv -- "${UPDATE_PACKAGE_FILE}" "${UPDATE_TEMP_FILE}"
			remove_sig_sidecars "${UPDATE_PACKAGE_FILE}"
			# Snapshot existing installation and arm the post-boot OTA health watchdog.
			create_ota_backup || true
			mkdir -p "${OTA_STATE_DIR}"
			printf 'package=%s\ntimestamp=%s\n' "${base_name}" "$(date -u +%FT%TZ)" > "${OTA_PENDING_FILE}"
			# Re-enable overlay for next boot before calling dpkg.
			rm -f /overlay/disable
			if STRATUX_OTA_INSTALL=1 DEBIAN_FRONTEND=noninteractive dpkg -i --force-depends --force-confnew "${UPDATE_TEMP_FILE}" >> "${STX_LOG}" 2>&1; then
				wLog "Package installed successfully. Post-boot OTA watchdog armed."
				rm -f -- "${UPDATE_TEMP_FILE}"
				do_reboot "Finished package installation. Rebooting..."
			else
				wLog "ERROR: dpkg failed to install ${UPDATE_PACKAGE_FILE}; restoring previous backup and preserving ${UPDATE_PACKAGE_FILE}.failed."
				restore_ota_backup "dpkg -i failed for ${base_name}"
				mv -- "${UPDATE_TEMP_FILE}" "${UPDATE_PACKAGE_FILE}.failed" || true
				do_reboot "Package installation failed and rolled back. Rebooting into read-only overlay..."
			fi
		fi
	fi
fi


if [ -f /boot/firmware/.stratux-first-boot ]; then
	rm -f /boot/firmware/.stratux-first-boot
	if [ -f /boot/firmware/stratux.conf ]; then
		# In case of US build, a stratux.conf file will always be imported, only containing UAT/OGN options.
		# We don't want to force-reboot for that.. Only for network/overlay changes
		do_reboot_flag=false

		# re-apply overlay
		if command -v jq >/dev/null 2>&1; then
			if [ "$(jq -r '.PersistentLogging // false' /boot/firmware/stratux.conf 2>/dev/null)" = "true" ]; then
				/sbin/overlayctl disable
				do_reboot_flag=true
				wLog "overlayctl disabled due to stratux.conf settings"
			fi
		else
			wLog "WARNING: jq not installed; skipping PersistentLogging import from stratux.conf"
		fi

		# write network config
		if grep -q WiFi /boot/firmware/stratux.conf; then
			/opt/stratux/bin/stratuxrun -write-network-config
			do_reboot_flag=true
			wLog "re-wrote network configuration for first-boot config import."
		fi
		if "${do_reboot_flag}"; then
			do_reboot "Rebooting after first-boot config import..."
		fi
	fi
fi

wLog "Exited without updating anything..."
