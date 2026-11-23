#!/bin/bash
set -euo pipefail

if VER="$(git describe --tags --abbrev=0 2>/dev/null)" && [ -n "${VER}" ]; then
	if [ "${VER:0:1}" = "v" ]; then
		VER="${VER:1}"
	fi
else
	SHORT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
	VER="0.0+${SHORT_SHA}"
fi
echo "${VER}"
