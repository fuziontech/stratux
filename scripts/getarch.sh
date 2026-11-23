#!/bin/bash
set -euo pipefail

ARCH="$(uname -m)"
case "${ARCH}" in
	x86_64)
		ARCH="amd64"
		;;
	aarch64)
		ARCH="arm64"
		;;
	armv7l|armv6l)
		ARCH="armhf"
		;;
esac
echo "${ARCH}"
