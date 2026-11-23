#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")"

echo "Fetching ddb.json from ddb.glidernet.org"
tmpfile="$(mktemp ddb.json.XXXXXX)"
trap 'rm -f -- "${tmpfile}"' EXIT

validate_json() {
	if command -v jq >/dev/null 2>&1; then
		jq -e . "$1" >/dev/null 2>&1
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c 'import json, sys; json.load(open(sys.argv[1], "r", encoding="utf-8"))' "$1" >/dev/null 2>&1
	else
		[ -s "$1" ]
	fi
}

if wget -qO "${tmpfile}" "https://ddb.glidernet.org/download/?j=1" && validate_json "${tmpfile}"; then
	mv -f -- "${tmpfile}" ddb.json
else
	echo "Error downloading or validating ddb.json. Using the existing copy stored in ddb.json.copy" >&2
	cp -f ddb.json.copy ddb.json
fi
exit 0
