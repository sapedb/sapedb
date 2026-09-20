GO ?= docker run --rm -v "$(PWD)":/src -w /src golang:1.24-alpine go

# VERSION is what a binary built here says when asked. It comes from the tag
# being built, so `sapedb version` answers with something that can be looked
# up, and two builds from two tags answer differently — which is the whole
# point, and is measured by building twice rather than by reading this line.
#
# --dirty, so a binary built over uncommitted changes says so instead of
# claiming the tag it is standing next to. --always, so a tree with no tags
# yet still produces something (the commit) rather than an empty string,
# which would stamp nothing and silently leave the binary saying "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The path here is internal/build.Path, spelled out because make cannot read a
# Go constant. Moving or renaming that package without changing this line does
# not fail the build — the linker accepts an -X for a symbol that does not
# exist and does nothing — so the guard is a test that stamps a binary through
# this same flag and refuses the default: see
# TestAStampedBuildSaysWhatItWasStampedWith.
STAMP := -X github.com/sapedb/sapedb/internal/build.Version=$(VERSION)

.PHONY: test vet build dist image run
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
build:
	$(GO) build -ldflags "$(STAMP)" ./...

# dist is the one that produces binaries somebody keeps. build only checks
# that the tree compiles.
dist:
	$(GO) build -trimpath -ldflags "-s -w $(STAMP)" -o bin/sapedb ./cmd/sapedb
	$(GO) build -trimpath -ldflags "-s -w $(STAMP)" -o bin/sapedbd ./cmd/sapedbd

image:
	docker build --build-arg VERSION="$(VERSION)" -t sapedb:latest .

run: image
	docker run --rm -p 7433:7433 \
		-e SAPEDB_SECRET="$${SAPEDB_SECRET:?set SAPEDB_SECRET}" \
		-e SAPEDB_INSECURE=1 \
		-v sapedb-data:/var/lib/sapedb \
		sapedb:latest
