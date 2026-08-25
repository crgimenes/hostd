# hostd ships two binaries with different destinations: hostctl runs on the
# operator's machine (macOS or Linux), hostd only on Linux hosts. The shared
# release.sh builds one binary per repository; the dist target is how this
# repository hands it that difference — and how anyone without the script
# builds the same artifacts.

VERSION ?= $(shell git describe --tags --always --dirty)
DIST_DIR ?= dist
LDFLAGS := -s -w -X main.Version=$(VERSION)

all: build

build:
	go build -trimpath ./...

test:
	go test -race -count 1 -timeout 400s ./...

# The daemon is built and zipped FIRST, into daemon/zips, so the hostctl
# built next embeds it: a released client installs the daemon of its own
# version, offline, with no version skew possible. The same zips are also
# published as separate release artifacts. hostctl darwin binaries stay
# uncompressed so release.sh can sign them after this runs.
dist: clean
	mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostd-linux-amd64 ./cmd/hostd
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostd-linux-arm64 ./cmd/hostd
	cd $(DIST_DIR) && zip -q -j hostd-linux-amd64.zip hostd-linux-amd64 && rm hostd-linux-amd64
	cd $(DIST_DIR) && zip -q -j hostd-linux-arm64.zip hostd-linux-arm64 && rm hostd-linux-arm64
	cp $(DIST_DIR)/hostd-linux-amd64.zip $(DIST_DIR)/hostd-linux-arm64.zip daemon/zips/
	printf '%s' "$(VERSION)" > daemon/zips/VERSION
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostctl-darwin-arm64 ./cmd/hostctl
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostctl-darwin-amd64 ./cmd/hostctl
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostctl-linux-amd64 ./cmd/hostctl
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/hostctl-linux-arm64 ./cmd/hostctl
	gzip -f $(DIST_DIR)/hostctl-linux-amd64 $(DIST_DIR)/hostctl-linux-arm64

# Every path here is a name this Makefile creates, and none is recursive:
# DIST_DIR is overridable, and `make clean DIST_DIR=/` must be a no-op rather
# than a lost machine. The globs are anchored to the artifact names, so a
# wrong DIST_DIR simply matches nothing. rmdir leaves the directory alone if
# anything else is in it, which is the answer somebody wants when they pointed
# it at a directory of their own.
clean:
	rm -f $(DIST_DIR)/hostctl-darwin-amd64 $(DIST_DIR)/hostctl-darwin-arm64
	rm -f $(DIST_DIR)/hostctl-linux-amd64 $(DIST_DIR)/hostctl-linux-arm64
	rm -f $(DIST_DIR)/hostctl-linux-amd64.gz $(DIST_DIR)/hostctl-linux-arm64.gz
	rm -f $(DIST_DIR)/hostd-linux-amd64 $(DIST_DIR)/hostd-linux-arm64
	rm -f $(DIST_DIR)/hostd-linux-amd64.zip $(DIST_DIR)/hostd-linux-arm64.zip
	rm -f daemon/zips/hostd-linux-amd64.zip daemon/zips/hostd-linux-arm64.zip daemon/zips/VERSION
	@rmdir $(DIST_DIR) 2>/dev/null || true

.PHONY: all build test dist clean
