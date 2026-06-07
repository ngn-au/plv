# Build on the native host arch and cross-compile to the target arch (no QEMU
# for the Go build). BUILDPLATFORM/TARGETOS/TARGETARCH are provided by buildx.
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine@sha256:f23e8b227fb4493eabe03bede4d5a32d04092da71962f1fb79b5f7d1e6c2a17f AS builder
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
FROM alpine:3.23@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11
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
