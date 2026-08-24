# syntax=docker/dockerfile:1

# --- Build ---------------------------------------------------------------
# The binary is built statically so the final image needs no libc.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/debugproxy ./cmd/debugproxy

# The data directory is created here so the runtime image carries it with the
# right ownership; a volume mounted over it inherits that ownership.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# --- Runtime -------------------------------------------------------------
# distroless/static ships the root certificates, which an HTTPS target needs.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="http-debug" \
      org.opencontainers.image.description="Local HTTP debug proxy with a live web interface" \
      org.opencontainers.image.source="https://github.com/DrSmithFr/http-debug" \
      org.opencontainers.image.licenses="Unlicense"

COPY --from=build /out/debugproxy /usr/local/bin/debugproxy
COPY --from=build --chown=65532:65532 /out/data /data

ENV LISTEN_ADDR=:8080 \
    WEB_ADDR=:8081 \
    DATA_DIR=/data

EXPOSE 8080 8081
VOLUME ["/data"]
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/debugproxy"]
