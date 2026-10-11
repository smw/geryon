#!/bin/sh
# Run by geryon when a player logs in: make sure their directories and rc
# file exist. $1 is the player's name.
set -eu
player=$1

mkdir -p /data/rcs/running "/data/rcs/ttyrecs/$player" "/data/rcs/$player"
if [ ! -f "/data/rcs/$player.rc" ]; then
    cp /data/current/../settings/init.txt "/data/rcs/$player.rc"
fi
