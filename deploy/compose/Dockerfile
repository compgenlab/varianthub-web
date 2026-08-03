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

# The React app, built first so its output can be embedded in the Go binary.
FROM node:22-alpine AS ui-build
WORKDIR /ui
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS web-build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Replace the placeholder embed dir with the real build.
COPY --from=ui-build /ui/embed/dist ./web/embed/dist
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
# bash, python3 and curl are here for build-recipe sources, not for the service.
#
# A source like REVEL ships a recipe that converts and merges its inputs before
# they can be indexed, and declares what it needs: requires = ["unzip",
# "python3"]. varhub preflights that list and refuses the download when
# something is missing, so an image without python3 cannot provision REVEL at
# all. unzip comes from busybox already.
#
# bash is not optional in the way the others are: varhub runs every recipe step
# as "bash -c 'set -euo pipefail; ...'" (internal/fetch/fetch.go), and pipefail
# is a bashism busybox sh does not accept. Alpine ships no bash, so without it
# no recipe runs at all — and unlike a declared requirement this one is not in
# the manifest, so the preflight cannot warn about it.
#
# The rest covers the common denominator, not everything: a recipe may require a
# tool nobody anticipated, and the honest answer there is the preflight error
# naming it rather than an image that tries to contain every possibility.
RUN apk add --no-cache ca-certificates tzdata bash python3 curl \
 && adduser -D -u 10001 varianthub

# Apptainer, for tool sources that run in a container (VEP, ANNOVAR).
#
# Alpine has no apptainer package, so it comes from the edge community repo
# pinned by that name rather than by version — this is a development image, and
# a broken pin here is a broken build rather than a broken deployment.
#
# Apptainer needs kernel features a default container does not get: it mounts
# SIF images through a loop device or FUSE. The compose file grants
# CAP_SYS_ADMIN, an unconfined seccomp profile, /dev/loop-control and /dev/fuse
# to match. On Kubernetes the same requirements become a securityContext with
# SYS_ADMIN and a device plugin for /dev/fuse — the part clusters most often
# refuse, and the reason this is opt-in rather than always on.
# libseccomp comes from edge as well: the edge apptainer links against symbols
# the 3.20 library does not export, and installing only the binary leaves it
# failing at exec with "symbol not found" rather than at build.
RUN apk add --no-cache \
      --repository=https://dl-cdn.alpinelinux.org/alpine/edge/main \
      --repository=https://dl-cdn.alpinelinux.org/alpine/edge/community \
      apptainer libseccomp fuse3 squashfs-tools squashfuse e2fsprogs \
 || echo "apptainer unavailable for this arch; tool sources will report it at run time"

# Apptainer bind-mounts /etc/localtime into every container it runs and fails
# outright when the source is missing. Alpine installs tzdata without creating
# the symlink, so the file has to exist before any tool source will start.
RUN ln -sf /usr/share/zoneinfo/UTC /etc/localtime
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
