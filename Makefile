# A crawl checkout built with `make WEBTILES=y`, for the integration tests.
CRAWL_SOURCE ?= ../crawl/crawl-ref/source

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test test-short check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o geryon ./cmd/geryon

test:
	GERYON_CRAWL_SOURCE=$(abspath $(CRAWL_SOURCE)) go test -race ./...

# Skips the test that waits out the twenty-second stale-process timeouts.
test-short:
	GERYON_CRAWL_SOURCE=$(abspath $(CRAWL_SOURCE)) go test -race -short ./...

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...

clean:
	rm -f geryon
