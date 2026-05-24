# Build on the native host arch and cross-compile to the target arch (no QEMU
# for the Go build). BUILDPLATFORM/TARGETOS/TARGETARCH are provided by buildx.
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS builder
WORKDIR /app

# download dependencies (cached layer)
COPY go.mod go.sum ./
RUN go mod download

# copy sources
COPY *.go ./
COPY web ./web

# cross-compile a static binary for the target platform
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o plv .

# create a new stage from alpine
FROM alpine:3.21
RUN apk add --no-cache tzdata

# copy the binary from the builder stage
COPY --from=builder /app/plv /usr/local/bin/plv

# expose the port 8080
# set the entrypoint to the plv binary
EXPOSE 8080
ENTRYPOINT ["plv"]
