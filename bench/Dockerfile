# The load generator, built from the same tree as the server it drives.
#
# Same shape as the server's Dockerfile and for the same reasons: build in one
# image, ship in nothing. It matters a little less here — this container holds
# no data and listens on no port — but a load generator that could be a shell
# is one more thing on the machine while a benchmark is running, and the point
# of the exercise is to know what is running.
#
# The context is the repository root, not this directory, because the load
# generator imports this repository's own client. That is the whole design:
# the numbers come from the same code a caller would use, at the same commit
# as the server, or they are numbers about something else.
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY sapedb.go ./
COPY internal ./internal
COPY bench ./bench

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /sapedb-bench ./bench

FROM scratch

COPY --from=build /sapedb-bench /sapedb-bench

ENTRYPOINT ["/sapedb-bench"]
