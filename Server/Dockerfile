FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
ARG TARGETOS
ARG TARGETARCH
ARG BT_BUILD_TAGS=""
ARG BT_SOURCE_SHA="unknown"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN build_tags=""; if [ -n "$BT_BUILD_TAGS" ]; then build_tags="-tags=$BT_BUILD_TAGS"; fi \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build $build_tags -trimpath -ldflags="-s -w -X main.sourceVersion=$BT_SOURCE_SHA" -o /out/baby-tracker-server ./cmd/baby-tracker-server \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build $build_tags -trimpath -ldflags='-s -w' -o /out/baby-tracker-admin ./cmd/baby-tracker-admin \
 && mkdir -p /state/security-ledger /state/ledger-head

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG BT_SOURCE_SHA="unknown"
LABEL org.opencontainers.image.revision=$BT_SOURCE_SHA
COPY --from=build /out/baby-tracker-server /out/baby-tracker-admin /usr/local/bin/
COPY --from=build --chown=65532:65532 /state/ /var/lib/babytracker/
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=2s --timeout=3s --start-period=5s --retries=15 \
  CMD ["/usr/local/bin/baby-tracker-server", "probe", "--url", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/usr/local/bin/baby-tracker-server"]
