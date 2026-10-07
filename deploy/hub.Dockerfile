# Build (needs BuildKit, which `docker buildx` always uses):
#   docker buildx build --platform linux/amd64 -f deploy/hub.Dockerfile -t petrvs-bridge-hub .
# Use --platform linux/arm64 for arm64 nodes. Without --platform the image is built for the machine that builds it, so
# one made on an Apple silicon Mac is arm64 and does not start on amd64 nodes ("exec format error").
# NOT YET BUILT OR RUN: a starting point. Pin the base images by digest before you use it.

# The Go compiler cross-compiles, so the build stage always runs on the builder's own architecture (no emulation) and
# only the binary is made for the target.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
# The official golang image sets GOTOOLCHAIN=local, so an image older than the `go` line of go.mod (a cached or mirrored
# 1.26.x, for example) stops with "go.mod requires go >= 1.26.6". With auto, Go fetches the toolchain that go.mod names.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/hub ./cmd/hub

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hub /hub
USER nonroot:nonroot
ENTRYPOINT ["/hub"]
