# Build:  docker build -f deploy/hub.Dockerfile -t petrvs-bridge-hub .
# NOT YET BUILT OR RUN: a starting point. Pin the base images by digest before you use it.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hub ./cmd/hub

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hub /hub
USER nonroot:nonroot
ENTRYPOINT ["/hub"]
