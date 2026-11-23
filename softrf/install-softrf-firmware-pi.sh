#!/bin/bash
set -euo pipefail
shopt -s nullglob

cd "$(dirname "$0")"

fwfiles=( SoftRF*.zip )
if (( ${#fwfiles[@]} != 1 )); then
    echo "ERROR: Expected exactly 1 SoftRF*.zip firmware archive, found ${#fwfiles[@]}" >&2
    exit 1
fi
fwfile="${fwfiles[0]}"
echo "${fwfile}"

if [ ! -f "USB-flashing-ESP32.zip" ]; then
    echo "ERROR: Missing USB-flashing-ESP32.zip" >&2
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
unzip -o -q -- "USB-flashing-ESP32.zip" -d "${tmpdir}"

for required_bin in bootloader_dio_80m.bin SoftRF.ino.partitions.bin boot_app0.bin; do
    if [ ! -f "${tmpdir}/${required_bin}" ]; then
        echo "ERROR: Expected ${required_bin} missing after extracting firmware archives" >&2
        exit 1
    fi
done

app_bins=( "${tmpdir}"/SoftRF.mb*.esp32.bin )
if (( ${#app_bins[@]} != 1 )); then
    echo "ERROR: Expected exactly 1 SoftRF.mb*.esp32.bin binary, found ${#app_bins[@]}" >&2
    exit 1
fi
app_bin="${app_bins[0]}"

systemctl stop stratux
stratux_stopped=1

esptool.py --chip auto --port /dev/serialin --baud 921600 erase_flash

esptool.py --chip auto --port /dev/serialin --baud 921600 --before default_reset --after hard_reset \
    write_flash -u --flash_mode dio --flash_freq 80m --flash_size detect \
    0x1000 "${tmpdir}/bootloader_dio_80m.bin" \
    0x8000 "${tmpdir}/SoftRF.ino.partitions.bin" \
    0xE000 "${tmpdir}/boot_app0.bin" \
    0x10000 "${app_bin}"
