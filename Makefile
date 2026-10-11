# A crawl checkout built with `make WEBTILES=y`. The browser client is copied
# from it, and the integration tests run its crawl binary.
CRAWL_SOURCE ?= ../crawl/crawl-ref/source

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build client test test-short check clean

build: client/version.txt
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o geryon ./cmd/geryon

# Refresh the embedded browser client. Needed once after cloning, and again
# whenever the crawl checkout changes.
client:
	scripts/sync-client.sh $(CRAWL_SOURCE)

client/version.txt:
	scripts/sync-client.sh $(CRAWL_SOURCE)

test: client/version.txt
	GERYON_CRAWL_SOURCE=$(abspath $(CRAWL_SOURCE)) go test -race ./...

# Skips the test that waits out the twenty-second stale-process timeouts.
test-short: client/version.txt
	GERYON_CRAWL_SOURCE=$(abspath $(CRAWL_SOURCE)) go test -race -short ./...

check: client/version.txt
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...

clean:
	rm -f geryon
