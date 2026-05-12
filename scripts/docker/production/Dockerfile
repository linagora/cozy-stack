# This Dockerfile should be used to create an environment to run cozy-stack
# in a production environment.


# Multi-stage image: this step builds cozy-stack
FROM golang:1.25-bookworm as build
WORKDIR /app

# Use docker layer caching to avoid redownloading go modules if the code has
# changed but not the dependencies.
COPY go.mod .
COPY go.sum .
RUN go mod download

# Build cozy-stack
COPY . .
# Build directly instead of going through scripts/build.sh, which runs
# `git describe` / `git rev-parse` and trips on the COPY'd working tree
# inside the container. The version string can be overridden at build
# time via --build-arg VERSION_STRING=...
ARG VERSION_STRING=docker-build
RUN BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
 && go build \
      -ldflags "-X github.com/cozy/cozy-stack/pkg/config.Version=${VERSION_STRING} \
        -X github.com/cozy/cozy-stack/pkg/config.BuildTime=${BUILD_TIME} \
        -X github.com/cozy/cozy-stack/pkg/config.BuildMode=production" \
      -o ./cozy-stack


# Multi-stage image: the main image
FROM node:20-bookworm-slim

ENV COUCHDB_PROTOCOL=http \
    COUCHDB_HOST=couchdb \
    COUCHDB_PORT=5984 \
    COUCHDB_USER=cozy \
    COUCHDB_PASSWORD=cozy

ARG DEBIAN_FRONTEND=noninteractive

COPY --from=build \
  /app/cozy-stack \
  /app/scripts/docker/production/docker-entrypoint.sh \
  /app/scripts/konnector-node-run.sh \
  /app/scripts/docker/production/wait-for-it.sh \
  /usr/local/bin/

RUN set -eux \
    && apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      gosu \
      git \
      imagemagick \
      ghostscript \
      librsvg2-bin \
      fonts-lato \
      postfix \
      jq \
    && postconf inet_interfaces=loopback-only \
    && postconf mydestination='$myhostname, localhost.localdomain, localhost' \
    && sed -ie 's,^  \(<policy domain="coder" rights="none" pattern="PDF" />\)$,  <!-- \1 -->,g' /etc/ImageMagick-6/policy.xml \
    && gosu nobody true \
    && apt-get autoremove -y && apt-get clean \
    && rm -rf /tmp/* /var/tmp /var/lib/apt/lists/* /var/cache/apt \
    && chmod +x /usr/local/bin/*.sh

WORKDIR /var/lib/cozy

VOLUME /var/lib/cozy/data
VOLUME /etc/cozy

EXPOSE 6060 8080

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["cozy-stack","serve"]
