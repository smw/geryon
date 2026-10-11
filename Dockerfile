# A webtiles server that keeps itself on current crawl trunk.
#
# The image holds geryon and what it takes to build crawl. Crawl itself is
# built at run time into the /data volume: once on first start, then again
# whenever trunk has moved (see docker/update-crawl.sh). Everything that must
# survive - accounts, saves, rc files, the crawl builds - is in /data.

FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /geryon ./cmd/geryon

FROM ubuntu:24.04
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates git procps util-linux \
      build-essential bison flex pkg-config python3-yaml advancecomp \
      libncursesw5-dev liblua5.4-dev libsqlite3-dev libz-dev libpng-dev \
      libsdl2-dev libsdl2-image-dev libsdl2-mixer-dev libfreetype6-dev \
 && rm -rf /var/lib/apt/lists/* \
 && userdel -r ubuntu \
 && useradd --uid 1000 --create-home crawl \
 && mkdir /data && chown crawl:crawl /data

COPY --from=build /geryon /usr/local/bin/geryon
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/geryon-entrypoint
COPY --chmod=755 docker/update-crawl.sh /usr/local/bin/geryon-update-crawl
COPY --chmod=755 docker/init-player.sh /usr/local/bin/geryon-init-player

# Crawl wants a UTF-8 locale.
ENV LANG=C.UTF-8
USER crawl
WORKDIR /data
VOLUME /data
# 8080: the game. 8081: admin pages and /metrics, which have no login.
EXPOSE 8080 8081
ENTRYPOINT ["geryon-entrypoint"]
