# geryon

A webtiles server for [Dungeon Crawl Stone Soup](https://github.com/crawl/crawl),
written in Go. It replaces the Python/Tornado server in
`crawl-ref/source/webserver` and serves crawl's own browser client unchanged;
crawl itself is not modified.

Named after the gatekeeper of the Vestibule of Hell.

## Status

Working for a personal or small shared server: accounts, lobby, playing,
spectating, chat, rc editing, in-progress locks and ttyrecs.

Not implemented: password reset by email, name ban lists, milestone files, the dgamelaunch status file, adopting games started
outside the server, and TLS (put a reverse proxy in front). `config.py` is
not read; see Configuration.

## Running with Docker

```sh
docker compose up -d
docker compose logs -f      # the first start builds crawl: several minutes
```

Then open <http://localhost:8080>.

The container builds crawl trunk itself and keeps it current. Once a day it
fetches crawl, and if trunk has moved it builds the new version in the
background and switches over: games started from then on use the new build,
and games in progress carry on undisturbed with the build they started on.
Saves are shared, and crawl upgrades an older trunk save when it loads it.

Everything that must survive is in the `geryon-data` volume: accounts, saves,
rc files, ttyrecs and the crawl builds. Server settings are in
`server/config.yml` in that volume, and you can add games of your own in
`server/games.d/` (the file `crawl.yaml` there is rewritten on every update).

`compose.yaml` documents the knobs: which branch, tag or commit to build, how
often to check, and extra `make` arguments such as `USE_DGAMELAUNCH=y`.

To update right away: `docker compose exec geryon geryon-update-crawl`.

## Running without Docker

You need a crawl checkout built with webtiles support. Run geryon from its
`crawl-ref/source` directory, as you would the Python server:

```sh
cd crawl/crawl-ref/source
make WEBTILES=y
/path/to/geryon             # listens on port 8080 by default
```

Stop the Python server first if it uses the same port. Existing accounts
(`webserver/passwd.db3`), rc files, saves and `webserver/games.d` definitions
are used as they are, and the browser client is served from `webserver/`.

Options: `-server-path DIR` (where `config.yml` and `games.d/` are; default
`./webserver`), `-port N`, `-address ADDR`, `-version`.

Signals: `SIGHUP` or `SIGUSR1` rereads the game definitions without
disturbing games in progress. `SIGINT` or `SIGTERM` saves every game and
exits.

## Admin pages and metrics

Set `admin_address` in `config.yml` (or pass `-admin`), for example
`127.0.0.1:8081`, to get a second, private listener:

- `/admin/` shows the games in progress, who is connected and the accounts,
  and lets you save and stop a game, disconnect someone, send an announcement
  to everyone in a game, create accounts (even with public registration off),
  set passwords, ban accounts or put them on hold, make someone an admin, and
  reload the game definitions. Each game links to a live view of its log:
  what the server did with it and everything crawl printed to stderr, which
  is where crawl reports errors and where Lua scripts print. Logs are kept in
  memory (the last 5000 lines) for running games and the last 20 that ended.
- `/metrics` is a Prometheus endpoint: connections by state, games running,
  spectators, games started and ended by reason, session lengths, logins,
  traffic before and after compression, slow clients dropped, plus the usual
  Go and process metrics. All of geryon's own series start with `geryon_`.

**These pages have no login.** Whoever can reach the address controls the
server, so bind it to loopback or a private network and never publish it. If
you put it behind a reverse proxy with its own host name, list that name in
`admin_allowed_hosts`; other host names are refused, as are form posts that
did not come from the admin page itself.

In the Docker setup the listener is on port 8081, published to the host's
loopback only: <http://localhost:8081/admin/>.

Accounts flagged as admin can also send announcements from the admin panel
of the game's own web client.

## Configuration

Settings are read from `config.yml` in the server path, with the same names
and meanings as the Python server's options. Games come from `games.d/*.yaml`
there. An option geryon does not act on is reported at startup.

Differences worth knowing:

- New password hashes are always SHA-512 (or SHA-256 with
  `crypt_algorithm: "5"`). Old DES hashes still verify.
- Several crawl versions can be offered side by side: give each game its own
  `crawl_binary` and `client_path`. Each version's game client is served and
  cached separately.

## Building and testing

```sh
make build        # a static binary, ./geryon
make check        # gofmt and go vet
make test         # needs a crawl checkout; about a minute
make test-short   # skips the test that waits out a twenty-second timeout
```

Most tests start the server in-process and play real crawl games through it
over WebSocket, as a player and as spectators. They use the crawl checkout at
`CRAWL_SOURCE` (default `../crawl/crawl-ref/source`), which must be built
with `make WEBTILES=y`. Run directly with `go test`, they are skipped unless
`GERYON_CRAWL_SOURCE` is set.

## How it is put together

- A **session** per browser connection handles that browser's requests.
- A **game** per crawl process relays between crawl and the browsers seated
  at it, and owns everything about that game: process, socket, roster, chat.
- The **hub** holds the little that is shared: the lobby's list of games, who
  is connected, login tokens.

Each session and game runs on its own goroutine. Waiting only goes one way
(session, then game, then hub), and output to browsers goes through a
per-connection outbox that never blocks the sender, so a slow browser or a
stuck game delays only itself.

## Continuous integration and releases

Every push runs the tests against crawl at the commit named in
`CRAWL_VERSION` (the build is cached), builds Linux archives for amd64 and
arm64, and builds the Docker image. Pushes to `main` publish the image as
`ghcr.io/smw/geryon:edge`.

Pushing a tag starting with `v` publishes the archives as a GitHub release
and the image as `:<tag>` and `:latest`:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

To test against a newer crawl, change the commit in `CRAWL_VERSION`.
