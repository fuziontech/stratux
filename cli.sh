#!/bin/bash
# Script for getting a cli interface into docker
set -euo pipefail

my_uid="${MY_UID:-$(id -u)}"
my_gid="${MY_GID:-$(id -g)}"

echo "Starting Stratux build shell in /data..."
MY_UID="${my_uid}" MY_GID="${my_gid}" docker compose run --rm cli
