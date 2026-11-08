# The Phase 5 load generator and seeder (baby-tracker-load, identitytest
# build only). The same pinned toolchain and distroless runtime as the server
# image (Server/Dockerfile). Built by load-node.sh as the `loadgen` service of
# Server/deploy/load/compose.yaml, with Server/ as the build context.
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags identitytest -trimpath -ldflags='-s -w' -o /out/baby-tracker-load ./cmd/baby-tracker-load

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/baby-tracker-load /usr/local/bin/baby-tracker-load
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/baby-tracker-load"]
