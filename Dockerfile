# Multi-arch build: docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 .
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src
COPY go.mod ./
COPY smartevse ./smartevse
COPY cmd ./cmd
RUN GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /smartevse-master ./cmd/smartevse-master

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /smartevse-master /smartevse-master
EXPOSE 7090
# Runs as uid 65532: give it the serial device's group (see examples/docker-compose.yaml).
ENTRYPOINT ["/smartevse-master"]
