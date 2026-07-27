# Build varianthub-web.
#
# NOTE: this image does NOT contain the varhub CLI, which the worker execs to do
# the actual annotation. Two ways to supply it:
#
#   dev  — compose bind-mounts a varhub binary from the host (see docker-compose.yml)
#   prod — add a stage here that fetches the varhub release binary once
#          varianthub-cli publishes one under its new name, e.g.
#            ADD https://github.com/compgenlab/varianthub-cli/releases/download/\
#                ${VARHUB_VERSION}/varhub-${VARHUB_VERSION}-linux-amd64 /usr/local/bin/varhub
#
# Leaving it out is deliberate: baking in a URL that 404s would be worse than an
# explicit gap.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache deps separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/varianthub-web ./cmd/varianthub-web

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 varianthub
COPY --from=build /out/varianthub-web /usr/local/bin/varianthub-web
USER varianthub
EXPOSE 8080
ENTRYPOINT ["varianthub-web"]
CMD ["serve"]
