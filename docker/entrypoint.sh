#!/bin/bash
# Container entry point: make sure there is a crawl build, keep it up to date
# in the background, and run geryon.
set -euo pipefail
cd /data

mkdir -p server/games.d rcs/running

if [ ! -f server/config.yml ]; then
    cat > server/config.yml <<'EOF'
# geryon configuration. Edit freely; this file is only written if missing.
# Option names are those of crawl's webtiles server (see geryon's README).
bind_port: 8080
password_db: /data/passwd.db3
init_player_program: /usr/local/bin/geryon-init-player
crypt_algorithm: "6"

# The current crawl build's copy of the browser client.
static_path: /data/current/webserver/static
template_path: /data/current/webserver/templates

# new_accounts_disabled: true
# allow_anon_spectate: false
EOF
fi

if [ ! -x current/crawl ]; then
    echo "No crawl build yet: building one now. This takes several minutes."
    geryon-update-crawl
fi
# Where geryon looks up the crawl version it reports to browsers.
ln -sfn /data/current/webserver/webtiles server/webtiles

interval=${CRAWL_UPDATE_INTERVAL:-86400}
if [ "$interval" -gt 0 ]; then
    (
        while sleep "$interval"; do
            geryon-update-crawl || echo "Crawl update failed; still serving the previous build."
        done
    ) &
fi

exec geryon -server-path /data/server
