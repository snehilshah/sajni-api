# Backend-only image. The React bundle is hosted on Vercel; this
# container serves /api/* plus health probes, nothing else.
#
# Build context expected to be `sajni-api/`. From repo root:
#   docker build -t sajni-api -f sajni-api/Dockerfile sajni-api

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sajni ./cmd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/sajni /usr/local/bin/sajni
EXPOSE 8080
# No --frontend flag — backend serves only /api/* + health probes now.
ENTRYPOINT ["sajni"]
