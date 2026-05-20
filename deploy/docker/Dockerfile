# syntax=docker/dockerfile:1.9

# ---------- Stage 1: build ----------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# Cross-compile target (set by buildx).
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# Pure-Go build (modernc.org/sqlite). Strip symbols and DWARF; trim
# absolute paths from the binary for reproducibility.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/northwatch ./cmd/northwatch

# Empty skeleton dir copied into the runtime image so SQLite has a
# writable home owned by UID 65532. Distroless has no shell/mkdir, so
# the directory must be staged here.
RUN mkdir -p /out/data

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/northwatch /usr/local/bin/northwatch
COPY --from=build --chown=65532:65532 /out/data /var/lib/northwatch
COPY deploy/docker/northwatch.default.yaml /etc/northwatch/northwatch.yaml

# Non-root by default (UID 65532 in distroless static nonroot).
USER nonroot:nonroot
WORKDIR /var/lib/northwatch

# Default SQLite location, so `docker run <image>` works without
# every caller having to set --db. Mount a volume at this path for
# persistence across restarts.
ENV NORTHWATCH_DB=/var/lib/northwatch/northwatch.db

# Point at the baked-in default config so the entrypoint succeeds
# without an external config mount. Override by mounting your own
# YAML at /etc/northwatch/northwatch.yaml or by setting --config.
ENV NORTHWATCH_CONFIG=/etc/northwatch/northwatch.yaml

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/northwatch"]
CMD ["serve"]
