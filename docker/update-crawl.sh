#!/bin/bash
# Bring the server up to date with crawl: fetch, build if there is something
# new, and switch new games over to the new build.
#
# Each build is installed in its own directory, /data/builds/<version>, and
# never modified afterwards. A game in progress keeps running the build it
# started with; only games started after the switch use the new one. Saves
# are shared, and crawl upgrades an older trunk save when it loads it.
#
# Environment:
#   CRAWL_REPO         repository to build from  (default: crawl's on GitHub)
#   CRAWL_REF          branch, tag or commit     (default: master)
#   CRAWL_MAKE_ARGS    extra arguments for make  (e.g. USE_DGAMELAUNCH=y)
#   CRAWL_KEEP_BUILDS  old builds to keep        (default: 3)
set -euo pipefail
cd /data

repo=${CRAWL_REPO:-https://github.com/crawl/crawl.git}
ref=${CRAWL_REF:-master}
keep=${CRAWL_KEEP_BUILDS:-3}

# One update at a time.
exec 9> /data/.update.lock
if ! flock -n 9; then
    echo "A crawl update is already running."
    exit 0
fi

if [ ! -d src/.git ]; then
    # All history but no old file contents: crawl takes its version from
    # `git describe`, which needs the tags and the commits between.
    git clone --filter=blob:none --no-checkout "$repo" src
fi
git -C src fetch --quiet --tags --prune origin
if git -C src rev-parse --verify --quiet "origin/$ref" > /dev/null; then
    target="origin/$ref"
else
    target="$ref"
fi
git -C src checkout --quiet --force --detach "$target"
version=$(git -C src describe --always)
build=/data/builds/$version
source=$build/crawl-ref/source

if [ -x "$source/crawl" ]; then
    echo "Crawl $version is already built."
else
    echo "Building crawl $version..."
    jobs=$(nproc)
    # shellcheck disable=SC2086
    build_crawl() { make -C src/crawl-ref/source -j"$jobs" WEBTILES=y ${CRAWL_MAKE_ARGS:-} > /data/build.log 2>&1; }
    # Crawl's Makefile has a race between generating and copying some client
    # files under -j; a second pass picks up where the first stopped.
    if ! build_crawl && ! build_crawl; then
        echo "Build failed. Last lines of /data/build.log:"
        tail -n 40 /data/build.log
        exit 1
    fi

    # Install: the binary and the data it reads, laid out as in the checkout
    # so that crawl finds its files relative to itself.
    stage=/data/builds/.staging
    rm -rf "$stage"
    mkdir -p "$stage/crawl-ref/source"
    cp -a src/crawl-ref/docs src/crawl-ref/settings "$stage/crawl-ref/"
    cp -a src/crawl-ref/source/crawl src/crawl-ref/source/dat src/crawl-ref/source/webserver \
        "$stage/crawl-ref/source/"
    rm -rf "$build"
    mv "$stage" "$build"
    echo "Installed crawl $version."
fi

# The games new sessions are offered. Other files in games.d are left alone.
mkdir -p server/games.d
cat > server/games.d/.crawl.yaml.new <<EOF
# Written by geryon-update-crawl on every update; changes here are lost.
# Add your own games in another file in this directory.
templates:
  - id: default
    crawl_binary: $source/crawl
    client_path: $source/webserver/game_data/
    rcfile_path: /data/rcs/
    macro_path: /data/rcs/
    morgue_path: /data/rcs/%n
    socket_path: /data/rcs
    inprogress_path: /data/rcs/running
    ttyrec_path: /data/rcs/ttyrecs/%n
    show_save_info: true
    allowed_with_hold: true
    version: trunk
games:
  - id: dcss-web-trunk
    name: Play %v
  - id: seeded-web-trunk
    name: Seeded
    options: [-seed]
  - id: descent-web-trunk
    name: Descent
    options: [-descent]
  - id: tut-web-trunk
    name: Tutorial
    options: [-tutorial]
  - id: sprint-web-trunk
    name: Sprint %v
    options: [-sprint]
EOF
mv server/games.d/.crawl.yaml.new server/games.d/crawl.yaml

previous=$(readlink current 2> /dev/null || true)
ln -sfn "$source" current.new
mv -T current.new current

if [ "$previous" != "$source" ] && pkill -HUP -x geryon; then
    echo "Told geryon to offer crawl $version for new games."
fi

# Remove old builds, but never the current one or one a game is still running.
in_use() {
    local exe
    for exe in /proc/[0-9]*/exe; do
        case "$(readlink "$exe" 2> /dev/null || true)" in "$1"/*) return 0 ;; esac
    done
    return 1
}
count=0
for old in $(ls -1dt /data/builds/*/ 2> /dev/null); do
    old=${old%/}
    [ "$old" = "$build" ] && continue
    count=$((count + 1))
    if [ "$count" -gt "$keep" ] && ! in_use "$old"; then
        echo "Removing old build ${old##*/}."
        rm -rf "$old"
    fi
done
