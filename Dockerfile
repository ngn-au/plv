FROM golang:1.26.1-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app

# copy the go.mod and go.sum files
COPY go.mod go.sum ./

# copy the go files
COPY *.go ./

# copy the web directory
COPY web ./web

# run the go mod tidy command
RUN go mod tidy

# build the binary
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o plv .

# create a new stage from alpine
FROM alpine:3.21
RUN apk add --no-cache tzdata

# copy the binary from the builder stage
COPY --from=builder /app/plv /usr/local/bin/plv

# expose the port 8080
# set the entrypoint to the plv binary
EXPOSE 8080
ENTRYPOINT ["plv"]
