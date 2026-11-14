FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/trawl .

FROM docker.io/library/alpine:3.24
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
 && mkdir -p /data /downloads \
 && chown 1000:1000 /data /downloads
COPY --from=build /out/trawl /usr/local/bin/trawl
ENV TRAWL_DATA=/data \
    TRAWL_FOLDERS=downloads:/downloads
EXPOSE 8080
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/trawl"]
