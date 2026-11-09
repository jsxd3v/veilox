# syntax=docker/dockerfile:1

# Build: a static binary for the target platform (no CGO).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o /out/veilox ./cmd/veilox \
 && mkdir -p /out/data

# Run: distroless, non-root, no shell.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/veilox /usr/local/bin/veilox
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /etc/veilox
ENV VEILOX_CONFIG=/etc/veilox/veilox.yaml \
    VEILOX_DB=/data/veilox.db \
    VEILOX_LISTEN=:8080
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/veilox"]
CMD ["serve"]
