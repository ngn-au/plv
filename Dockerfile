# Build on the native host arch and cross-compile to the target arch (no QEMU
# for the Go build). BUILDPLATFORM/TARGETOS/TARGETARCH are provided by buildx.
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS builder
WORKDIR /app

# download dependencies (cached layer)
COPY go.mod go.sum ./
RUN go mod download

# copy sources
COPY *.go ./
COPY web ./web

# cross-compile a static binary for the target platform.
# VERSION is injected into the binary via -ldflags (defaults to "dev").
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o plv .

# create a new stage from alpine
FROM alpine:3.23
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
