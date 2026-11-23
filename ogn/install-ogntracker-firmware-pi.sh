#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")"

if [ "$#" -lt 1 ] || [ -z "$1" ]; then
    echo "Usage: $0 <firmware-basename>" >&2
    exit 1
fi

fwname="$(basename -- "$1")"
if [ "${fwname}" != "$1" ] || [[ ! "${fwname}" =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "Invalid firmware name: $1" >&2
    exit 1
fi

fwfile="${fwname}.zip"
echo "${fwfile}"
if [ ! -f "${fwfile}" ]; then
    echo "Unknown firmware ${fwname}" >&2
    exit 1
fi

tmpdir="$(mktemp -d)"
stratux_stopped=0
cleanup() {
    rm -rf -- "${tmpdir}"
    if [ "${stratux_stopped}" -eq 1 ]; then
        systemctl start stratux || true
    fi
}
trap cleanup EXIT

unzip -o -q -- "${fwfile}" -d "${tmpdir}"

for required_bin in bootloader.bin firmware.bin partitions.bin; do
    if [ ! -f "${tmpdir}/${required_bin}" ]; then
        echo "ERROR: Expected ${required_bin} missing from ${fwfile}" >&2
        exit 1
    fi
done

systemctl stop stratux
stratux_stopped=1

bootloader_offset="0x1000"
if [[ "${fwname}" == *tbeams3* ]]; then
    bootloader_offset="0x0"
elif esptool.py --port /dev/serialin chip_id 2>/dev/null | grep -qi "ESP32-S3"; then
    bootloader_offset="0x0"
fi

echo "Flashing ${fwfile} with bootloader offset ${bootloader_offset}..."
esptool.py --chip auto --port /dev/serialin --baud 921600 --before default_reset --after hard_reset \
    write_flash -u --flash_mode dio --flash_freq 40m --flash_size detect \
    "${bootloader_offset}" "${tmpdir}/bootloader.bin" \
    0x10000 "${tmpdir}/firmware.bin" \
    0x8000 "${tmpdir}/partitions.bin"
