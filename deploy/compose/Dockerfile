# syntax=docker/dockerfile:1.7

# varianthub-web plus the varhub CLI its worker execs.
#
# Both binaries live in one image on purpose: the worker is useless without the
# CLI, and a bind-mounted host binary (the previous approach) breaks the moment
# the image runs anywhere but this machine.
#
# varhub is built from a named build context, `varhub-src`, so the dev stack
# compiles the CLI you actually have checked out rather than a published release:
#
#   docker build --build-context varhub-src=../../../cganno \
#     -f deploy/compose/Dockerfile ..
#
# compose supplies it via additional_contexts (see docker-compose.yml). For
# production, swap this stage for one that fetches a released varhub binary once
# varianthub-cli publishes one under its new name.

FROM golang:1.25-alpine AS web-build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/varianthub-web ./cmd/varianthub-web

FROM golang:1.25-alpine AS varhub-build
WORKDIR /src
# GOFLAGS=-mod=mod and no go.work: the CLI's Makefile sets GOWORK=off because the
# developer's parent go.work references modules that do not exist in the image.
ENV GOWORK=off
COPY --from=varhub-src . .
RUN --mount=type=cache,target=/go/pkg/mod go mod download
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/varhub ./cmd/varhub

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 varianthub
COPY --from=web-build    /out/varianthub-web /usr/local/bin/varianthub-web
COPY --from=varhub-build /out/varhub         /usr/local/bin/varhub

# Shared, persistent storage for downloaded source files and built indexes. The
# per-job annotation config is materialized into a temp dir instead, so there is
# nothing to mount for it.
RUN mkdir -p /var/lib/varianthub && chown varianthub:varianthub /var/lib/varianthub

USER varianthub
EXPOSE 8080
ENTRYPOINT ["varianthub-web"]
CMD ["serve"]
