# Build on the native host arch and cross-compile to the target arch (no QEMU
# for the Go build). BUILDPLATFORM/TARGETOS/TARGETARCH are provided by buildx.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder
WORKDIR /app

# download dependencies (cached layer)
COPY go.mod go.sum ./
RUN go mod download

# copy sources
COPY *.go ./
COPY web ./web

# cross-compile a static binary for the target platform.
# Build provenance for the version chip (see buildmeta.go) is injected via -ldflags:
#   GIT_SHA — short commit SHA → a "commit" build (chip shows 1.0.1+abc1234)
#   RELEASE — "true" on a vX.Y.Z tag build → a "release" build (chip shows 1.0.1)
# Both empty (a plain `docker build` with no build-args) → a "dev" build (1.0.1-dev).
ARG TARGETOS TARGETARCH
ARG GIT_SHA=""
ARG RELEASE=""
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.gitSha=${GIT_SHA} -X main.releaseBuild=${RELEASE}" -o plv .

# create a new stage from alpine
FROM alpine:3.24@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4
RUN apk add --no-cache tzdata

# OCI image metadata (the publish workflow adds source/revision labels too).
LABEL org.opencontainers.image.title="plv" \
      org.opencontainers.image.description="PLV — Postfix Log Viewer" \
      org.opencontainers.image.source="https://github.com/ngn-au/plv" \
      org.opencontainers.image.licenses="MIT"

# copy the binary from the builder stage
COPY --from=builder /app/plv /usr/local/bin/plv

# expose the port 8080
# set the entrypoint to the plv binary
EXPOSE 8080
ENTRYPOINT ["plv"]
