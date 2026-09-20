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

.PHONY: test vet build dist dist-cross checksums verify-dist image run
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

# CROSS_PLATFORMS is every platform a stranger cloning this repository is
# plausibly going to run it on, and nothing else.
#
# linux/amd64 and linux/arm64 are where this actually runs in production:
# the Dockerfile's own base and runtime stages are Linux, and both
# architectures are ordinary on cloud VMs today, not just amd64.
#
# darwin/amd64 and darwin/arm64 are where a stranger *evaluating* this
# without Docker runs it — an Intel or an Apple Silicon laptop, both still
# common development machines.
#
# Nothing else. No Windows: no test in this repository has ever exercised a
# Windows path or its file-locking behaviour (internal/vfs assumes POSIX
# semantics throughout), and a binary nobody has verified boots is worse
# than a platform nobody offered. No 32-bit or non-amd64/arm64 Linux target:
# a database server here is not something anyone plausibly deploys to
# 32-bit hardware, and shipping an artifact this repository cannot test is
# a liability, not a convenience.
CROSS_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# dist-cross builds both binaries for every platform in CROSS_PLATFORMS,
# stamped exactly as `dist` stamps the host build — same STAMP, same
# -trimpath, same -s -w. It uses the host's own `go` toolchain directly
# rather than $(GO)'s docker wrapper: cross-compiling through a container
# only pinned to the host's own OS/arch buys nothing, since Go's compiler
# cross-compiles natively given GOOS/GOARCH, and CI runs this with a `go`
# already installed by actions/setup-go.
#
# It is a separate target rather than a loop folded into `dist` so that a
# developer building for themselves keeps getting exactly what they always
# got: two binaries, for their own machine, without waiting on four
# platforms they are not going to run.
dist-cross:
	@mkdir -p dist
	@for platform in $(CROSS_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "==> building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(STAMP)" -o dist/sapedb_$(VERSION)_$${os}_$${arch} ./cmd/sapedb || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(STAMP)" -o dist/sapedbd_$(VERSION)_$${os}_$${arch} ./cmd/sapedbd || exit 1; \
	done

# checksums covers every artifact dist-cross just produced, in one file, in
# the plain `<hex>  <filename>` format both sha256sum -c and shasum -a 256 -c
# read — so a stranger verifies a download with whichever of the two their
# OS ships, and neither this target nor that filename list depends on which
# of the two tools generated it.
checksums:
	@cd dist && ls sapedb_$(VERSION)_* sapedbd_$(VERSION)_* >/dev/null && ( \
		if command -v sha256sum >/dev/null 2>&1; then \
			sha256sum sapedb_$(VERSION)_* sapedbd_$(VERSION)_* > checksums.txt; \
		else \
			shasum -a 256 sapedb_$(VERSION)_* sapedbd_$(VERSION)_* > checksums.txt; \
		fi )
	@cat dist/checksums.txt

# verify-dist is the guard SAPE-1's ISS-18 exists to name: `go build` accepts
# an -ldflags -X for a symbol that does not exist and silently does nothing,
# so a build that compiles and a build that actually stamped VERSION are two
# different claims, and only one of them is checked by the build succeeding.
#
# It does not trust dist-cross's own exit code. It re-derives the version
# from the bytes each artifact actually contains, two ways:
#
#   - the artifact matching this machine's own GOOS/GOARCH is executed and
#     asked directly (`sapedb version`), which is the strongest check there
#     is, and is why at least one artifact here must match the host: a run
#     that found nothing to execute has verified nothing, not everything;
#   - every other artifact cannot be executed here — a linux/arm64 binary
#     does not run on a darwin/arm64 host, or in a linux/amd64 CI runner
#     either — so it is instead grepped for the literal stamped string.
#     -ldflags -X sets a Go string variable, and strip (-s -w) removes the
#     symbol table and debug info, not the string data the running program
#     reads at request time; that text is still there in cleartext, which is
#     exactly what the grep below reads back.
#
# grep -F, not grep: a version like "v1.2.3-4-g<hash>" has no regex
# metacharacters that matter here, but a bare grep is one stray "." or "$"
# away from matching more, or less, than the literal string, and that
# mistake has already cost this project time once today.
# sapedb (the CLI) has a `version` subcommand to ask directly. sapedbd (the
# daemon) has none — its only argument surface is the environment, and its
# answer to "which build is this" is the first line of its own log (see
# internal/service.Serve) — so the host-arch check for it is: start it with
# just enough environment to pass its own startup checks, read that first
# line, and stop it. Both are "execute the binary and read the version back
# from its own mouth"; they just knock on two different doors.
verify-dist:
	@echo "checking VERSION=$(VERSION)"
	@host_os=$$(go env GOOS); host_arch=$$(go env GOARCH); \
	executed=0; \
	for f in dist/sapedb_$(VERSION)_* dist/sapedbd_$(VERSION)_*; do \
		[ -e "$$f" ] || continue; \
		base=$$(basename "$$f"); \
		case "$$f" in \
			*_$${host_os}_$${host_arch}) \
				chmod +x "$$f"; \
				case "$$base" in \
					sapedbd_*) \
						tmp=$$(mktemp -d); log="$$tmp/log"; \
						(SAPEDB_SECRET=verify-dist-only SAPEDB_INSECURE=1 SAPEDB_DIR="$$tmp/data" SAPEDB_ADDR="127.0.0.1:0" "./$$f" >"$$log" 2>&1 &); \
						sleep 1; \
						pid=$$(pgrep -f "$$f" | head -1); \
						out=$$(cat "$$log" 2>/dev/null || echo ""); \
						[ -n "$$pid" ] && kill "$$pid" 2>/dev/null || true; \
						rm -rf "$$tmp"; \
						case "$$out" in \
							*"$(VERSION)"*) executed=$$((executed+1)); echo "ok (executed)  $$f: $$(echo "$$out" | head -1)" ;; \
							*) echo "FAIL $$f: startup log was: $$out"; exit 1 ;; \
						esac ;; \
					*) \
						got=$$("./$$f" version 2>&1) || { echo "FAIL $$f: would not run: $$got"; exit 1; }; \
						case "$$got" in \
							*"$(VERSION)"*) executed=$$((executed+1)); echo "ok (executed)  $$f: $$got" ;; \
							*) echo "FAIL $$f: printed $$got, which does not contain $(VERSION)"; exit 1 ;; \
						esac ;; \
				esac ;; \
			*) \
				if grep -qaF -- "$(VERSION)" "$$f"; then \
					echo "ok (inspected) $$f: contains $(VERSION)"; \
				else \
					echo "FAIL $$f: does not contain $(VERSION) anywhere in the file"; exit 1; \
				fi ;; \
		esac; \
	done; \
	if [ "$$executed" -lt 2 ]; then \
		echo "FAIL: expected to execute one sapedb and one sapedbd artifact for this host's $${host_os}/$${host_arch}, actually executed $$executed"; \
		exit 1; \
	fi

image:
	docker build --build-arg VERSION="$(VERSION)" -t sapedb:latest .

run: image
	docker run --rm -p 7433:7433 \
		-e SAPEDB_SECRET="$${SAPEDB_SECRET:?set SAPEDB_SECRET}" \
		-e SAPEDB_INSECURE=1 \
		-v sapedb-data:/var/lib/sapedb \
		sapedb:latest
