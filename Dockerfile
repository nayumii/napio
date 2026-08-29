# Build
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/napio ./cmd/napio

# Run
#
# napio needs no filesystem, no shell and no CA certificates: it talks to the
# Docker socket and to RCON, both unencrypted local endpoints. Scratch keeps
# the image at a few megabytes and gives it nothing to be exploited with.
FROM scratch
COPY --from=build /out/napio /napio

# Runs unprivileged. Access to the Docker socket is granted by the host, and
# host networking is required so napio can hold the game's UDP port.
USER 65532:65532

ENTRYPOINT ["/napio"]
CMD ["-config", "/etc/napio/napio.yaml"]
