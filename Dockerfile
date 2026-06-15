# Backend-only image. The React bundle is hosted on Vercel; this
# container serves /api/* plus health probes, nothing else.
#
# Build context expected to be `sajni-api/`. From repo root:
#   docker build -t sajni-api -f sajni-api/Dockerfile sajni-api

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# proxy.golang.org occasionally resets the HTTP/2 stream mid-zip
# ("INTERNAL_ERROR; received from peer"). Force HTTP/1.1 to dodge it and
# retry a few times for any other transient blip. GOPROXY keeps its default
# ",direct" fallback.
RUN GODEBUG=http2client=0 sh -c 'for i in 1 2 3 4 5; do go mod download && exit 0; echo "go mod download retry $i"; sleep 3; done; exit 1'
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sajni ./cmd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/sajni /usr/local/bin/sajni
EXPOSE 8080
# No --frontend flag — backend serves only /api/* + health probes now.
ENTRYPOINT ["sajni"]
