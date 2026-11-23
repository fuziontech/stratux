#!/bin/bash
set -euo pipefail

if [ "$#" -lt 1 ]; then
    echo "Usage: $0 <logfile>" >&2
    exit 1
fi

grep -a -v '^START' "$1" | grep -a -v '^PAUSE' | grep -a -v '^UNPAUSE' | grep -a 'PUBX,00' | cut -d, -f1,5,6,7,8,9
