#!/bin/bash
set -euo pipefail

my_uid="${MY_UID:-$(id -u)}"
my_gid="${MY_GID:-$(id -g)}"

if [ "$#" -eq 0 ]; then
	MY_UID="${my_uid}" MY_GID="${my_gid}" docker compose run --service-ports --rm cli
elif [ "$#" -eq 1 ]; then
	MY_UID="${my_uid}" MY_GID="${my_gid}" docker compose run --service-ports --rm cli -c "$1"
else
	MY_UID="${my_uid}" MY_GID="${my_gid}" docker compose run --service-ports --rm cli -c 'exec "$@"' _ "$@"
fi
