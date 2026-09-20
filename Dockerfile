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

# VERSION is what the server inside this image says it is, in the welcome it
# sends every client. The default is what an unstamped build says anyway, so
# an image built without --build-arg is honest about being unidentifiable
# rather than claiming a release it is not. `make image` passes the real one.
#
# .git is not copied into this stage on purpose — it is not source, and the
# build must not depend on it — so the version comes in as an argument rather
# than being discovered here.
ARG VERSION=dev

# Static: the runtime image has no dynamic loader to find a library with.
# Stripped: the symbol table is of no use in production and is of some use to
# whoever is reading the binary.
#
# The -X path is internal/build.Path. Docker cannot read a Go constant either;
# the same test guards this line as guards the Makefile's.
ENV CGO_ENABLED=0
RUN go build -trimpath \
	-ldflags="-s -w -X github.com/sapedb/sapedb/internal/build.Version=${VERSION}" \
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
