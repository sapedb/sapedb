# Build in one image, ship in nothing.
#
# The result holds the binary, an empty data directory owned by a non-root
# user, and not one thing else — no shell, no package manager, no libc. A
# database server is exactly the process somebody who gets in wants a shell
# from, and the cheapest way to not give them one is to not ship one.
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY cmd ./cmd
COPY scripts ./scripts

# VERSION is what the server inside this image says it is, in the welcome it
# sends every client. The default is what an unstamped build says anyway, so
# an image built without --build-arg is honest about being unidentifiable
# rather than claiming a release it is not. `make image` passes the real one.
#
# .git is not copied into this stage on purpose — it is not source, and the
# build must not depend on it — so the version comes in as an argument rather
# than being discovered here.
ARG VERSION=dev

# STAMP is this file's one spelling of internal/build.Path, written once and
# read by both steps below — the check and the build — so the two cannot
# disagree with each other, only with Go. Docker cannot read a Go constant any
# more than make can; the same test guards this line as guards the Makefile's.
ARG STAMP="-X github.com/sapedb/sapedb/internal/build.Version=${VERSION}"

# The linker accepts an -X for a symbol that does not exist, does nothing with
# it, and exits 0, so an image built after a rename would ship a binary calling
# itself "dev" and nothing here would have said a word (ISS-18). This resolves
# the line above through the Go toolchain first, and fails the build if it
# names nothing.
RUN ./scripts/check-stamp-symbol.sh "$STAMP"

# Static: the runtime image has no dynamic loader to find a library with.
# Stripped: the symbol table is of no use in production and is of some use to
# whoever is reading the binary.
ENV CGO_ENABLED=0
RUN go build -trimpath \
	-ldflags="-s -w $STAMP" \
	-o /sapedbd ./cmd/sapedbd

# The data directory is made here, with its ownership, because the runtime
# image has no shell to make one in.
RUN mkdir -p /data && chown 65532:65532 /data

FROM scratch

COPY --from=build /sapedbd /sapedbd
COPY --from=build --chown=65532:65532 /data /var/lib/sapedb

# Numeric, because there is no /etc/passwd here to hold a name. 65532 is the
# convention distroless uses for "nonroot".
USER 65532:65532

# Databases live here and must outlast the container.
VOLUME ["/var/lib/sapedb"]

EXPOSE 7433

# No HEALTHCHECK on purpose. A check this image could run without credentials
# only proves the port accepts, which an orchestrator's TCP probe already does;
# a check that proves a database is answering needs a signed connection string,
# which does not belong baked into an image. A health check that passes while
# the thing it checks is broken is worse than none.
ENTRYPOINT ["/sapedbd"]
