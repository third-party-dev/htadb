# Dev/build image for htadb: Go toolchain, adb, headless Chromium, ffmpeg, node (vsce).
FROM alpine:edge
RUN apk add --no-cache go git make bash curl ca-certificates android-tools chromium font-dejavu font-noto ffmpeg nodejs npm zip unzip jq
ENV GOPATH=/go GOMODCACHE=/go/pkg/mod GOCACHE=/go/cache PATH=/go/bin:$PATH CGO_ENABLED=0
WORKDIR /src
