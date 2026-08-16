ARG WEB_BUILD_IMAGE=node:24.19.0-bookworm-slim
ARG GO_BUILD_IMAGE=golang:1.26.6-bookworm

FROM ${WEB_BUILD_IMAGE} AS web-build

WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM ${GO_BUILD_IMAGE} AS go-build

ARG GOVULNCHECK_VERSION=v1.6.0
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
RUN --mount=type=cache,target=/go/pkg/mod GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}
ARG FLEET_BUILD_ID=development
ARG FLEET_VULN_CHECK_NONCE=manual
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY runtime/ ./runtime/
RUN --mount=type=cache,target=/go/pkg/mod test -n "${FLEET_VULN_CHECK_NONCE}" && govulncheck ./...
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/jacobcalvyn/hermes-fleet-manager/internal/api.BuildID=${FLEET_BUILD_ID}" \
    -o /out/hermes-fleet-control-plane ./cmd/control-plane
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/hermes-fleet-agent ./cmd/host-agent
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/fleet-upgrade-guard ./cmd/fleet-upgrade-guard
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/hermes-release-catalog ./cmd/hermes-release-catalog
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/fleet-recovery-import ./cmd/fleet-recovery-import
RUN govulncheck -mode=binary /out/hermes-fleet-control-plane \
    && govulncheck -mode=binary /out/hermes-fleet-agent \
    && govulncheck -mode=binary /out/fleet-upgrade-guard \
    && govulncheck -mode=binary /out/hermes-release-catalog \
    && govulncheck -mode=binary /out/fleet-recovery-import

FROM scratch AS host-tools
COPY --from=go-build /out/hermes-fleet-agent /hermes-fleet-agent
COPY --from=go-build /out/fleet-upgrade-guard /fleet-upgrade-guard
COPY --from=go-build /out/hermes-release-catalog /hermes-release-catalog

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home fleet \
    && mkdir -p /var/lib/hermes-fleet /var/lib/hermes-fleet-cloudflare/admin /var/lib/hermes-fleet-cloudflare/instances /app/web \
    && chown -R fleet:fleet /var/lib/hermes-fleet /var/lib/hermes-fleet-cloudflare /app

COPY --from=go-build /out/hermes-fleet-control-plane /usr/local/bin/hermes-fleet-control-plane
COPY --from=go-build /out/fleet-recovery-import /usr/local/bin/fleet-recovery-import
COPY --from=web-build /src/web/dist/ /app/web/

USER fleet
EXPOSE 9180
ENTRYPOINT ["hermes-fleet-control-plane"]
