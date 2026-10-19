# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rightsizing-check ./cmd/rightsizing-check

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/robertasolimandonofreo/rightsizing-check" \
      org.opencontainers.image.description="Find inflated Kubernetes CPU and memory requests, offline" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/rightsizing-check /rightsizing-check
ENTRYPOINT ["/rightsizing-check"]
