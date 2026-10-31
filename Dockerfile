# Multi-stage build: static Go binaries on a minimal Alpine runtime.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/panel ./cmd/panel \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

FROM alpine:3.20 AS panel
RUN addgroup -g 10001 panel && adduser -D -H -u 10001 -G panel panel && mkdir -p /data && chown panel:panel /data
COPY --from=build /out/panel /usr/local/bin/panel
USER panel
ENV PANEL_ADDR=":8080" PANEL_DATA_DIR="/data" PANEL_SECURE_COOKIES="true"
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/panel"]

FROM alpine:3.20 AS agent
# The agent runs on the HOST as a systemd service in the recommended
# deployment (it needs the Docker socket and host site directories).
# This image exists for hosts that prefer a containerised agent with the
# Docker socket mounted into the AGENT ONLY - never into the panel.
COPY --from=build /out/agent /usr/local/bin/agent
ENTRYPOINT ["/usr/local/bin/agent"]
