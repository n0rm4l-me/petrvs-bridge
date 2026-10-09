# Build (needs BuildKit, which `docker buildx` always uses):
#   docker buildx build --platform linux/amd64 -f deploy/hub.Dockerfile -t petrvs-bridge-hub .
# Use --platform linux/arm64 for arm64 nodes. Without --platform the image is built for the machine that builds it, so
# one made on an Apple silicon Mac is arm64 and does not start on amd64 nodes ("exec format error").
# NOT YET BUILT OR RUN INSIDE A CONTAINER: a starting point. The final base image is pinned by digest; pin the golang image of the build stage too before you use it.

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

# Pinned by the digest of the index (multi-architecture); take a new one on purpose, by reading its digest, and not by pulling the tag.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/hub /hub
USER nonroot:nonroot
ENTRYPOINT ["/hub"]
