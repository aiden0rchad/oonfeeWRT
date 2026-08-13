# oonfeeWRT controller image — multi-stage, final stage FROM scratch.
# Build from the repo root:
#   docker buildx build -f deploy/Dockerfile \
#     --platform linux/amd64,linux/arm64 -t oonfeewrt:dev .
#
# Budget (CI-enforced): final image ≤ 40 MB. No shell, no package manager,
# no libc in the final stage — one static binary is the whole attack surface.

# ---- stage 1: UI ----------------------------------------------------------
FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
# vite build emits ui/dist plus precompressed .gz siblings (vite plugin);
# budget: ≤ 1.5 MB gzipped total (checked again in CI by tools/budget_check.sh)
RUN npm run build

# ---- stage 2: controller --------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
# CGO off (decision D3) → fully static, works FROM scratch on any arch.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /oonfeewrtd ./cmd/oonfeewrtd

# ---- stage 3: runtime -----------------------------------------------------
FROM scratch
# CA roots (device HTTPS + webhook notifications) and timezone data (schedules).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=alpine:3.20 /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /oonfeewrtd /oonfeewrtd

# Non-root. /data must be chown'd 65532 by the volume or compose (user: below).
USER 65532:65532
VOLUME ["/data"]
ENV OONFEE_DATA_DIR=/data \
    OONFEE_LISTEN=:8080
EXPOSE 8080 8443
ENTRYPOINT ["/oonfeewrtd"]
