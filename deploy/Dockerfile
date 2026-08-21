# oonfeeWRT controller image — multi-stage, final stage FROM scratch.
# Build from the repo root:
#   docker buildx build -f deploy/Dockerfile \
#     --platform linux/amd64,linux/arm64 -t oonfeewrt:dev .
#
# Budget (CI-enforced): final image ≤ 40 MB. No shell, no package manager,
# no libc in the final stage — one static binary is the whole attack surface.

# ---- stage 1: UI ----------------------------------------------------------
# Pinned to the BUILD platform on purpose: without it, buildx runs this whole
# stage under QEMU once per target arch, and `npm ci` under emulation is both
# slow and flaky. The output is static assets, so it is arch-independent.
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
# vite build emits ui/dist plus precompressed .gz siblings (vite plugin);
# budget: ≤ 1.5 MB gzipped total (checked again in CI by tools/budget_check.sh)
RUN npm run build

# ---- stage 2: controller --------------------------------------------------
# Also on BUILDPLATFORM: the Go toolchain runs natively and cross-compiles via
# GOOS/GOARCH below, which is what decision D3 (CGO off) buys us. Leaving this
# unpinned would emulate the whole toolchain and make TARGETOS/TARGETARCH a
# no-op, since they would just restate the emulated platform.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
# CGO off (decision D3) → fully static, works FROM scratch on any arch.
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X main.version=$VERSION" \
      -o /oonfeewrtd ./cmd/oonfeewrtd
# Materialised here so the scratch stage can COPY them in with an owner —
# see the /data note below.
RUN mkdir -p /emptydirs/data /emptydirs/tmp

# ---- stage 2b: timezone database ------------------------------------------
# tzdata is a separate package; the alpine BASE image ships no /usr/share/
# zoneinfo, so copying it straight from `alpine:3.20` fails the build outright.
# Alternative worth taking once cmd/oonfeewrtd exists: a blank `time/tzdata`
# import embeds the database in the binary (~450 KB) and removes this stage.
FROM alpine:3.20 AS tz
RUN apk add --no-cache tzdata

# ---- stage 3: runtime -----------------------------------------------------
FROM scratch
# CA roots (device HTTPS + webhook notifications) and timezone data (schedules).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=tz /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /oonfeewrtd /oonfeewrtd

# scratch has no /data, so Docker would create the mount point as root:root and
# a fresh named volume would inherit that — leaving the non-root daemon unable
# to create the SQLite DB, with no shell in the image to repair it. `VOLUME`
# does not help; only a directory that exists in the image with the right owner
# does. /tmp likewise: scratch has none and SQLite wants one.
COPY --from=build --chown=65532:65532 /emptydirs/data /data
COPY --from=build --chown=65532:65532 /emptydirs/tmp /tmp
USER 65532:65532
VOLUME ["/data"]
ENV OONFEE_DATA_DIR=/data \
    OONFEE_LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["/oonfeewrtd"]
