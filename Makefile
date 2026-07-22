GO      ?= go
LDFLAGS  = -ldflags "-s -w" -trimpath
PKG      = ./cmd/tracker

.PHONY: build build-all test clean

# Host-only build, for development.
build:
	CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o tracker $(PKG)

test:
	$(GO) test ./...

# Cross-compiled binaries committed to the repo — a marketplace install
# is a git clone and must work with no bootstrap download.
build-all:
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/tracker-darwin-arm64      $(PKG)
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/tracker-darwin-amd64      $(PKG)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/tracker-linux-amd64       $(PKG)
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/tracker-linux-arm64       $(PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/tracker-windows-amd64.exe $(PKG)

clean:
	rm -f tracker
