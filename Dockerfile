# syntax=docker/dockerfile:1
# check=skip=InvalidDefaultArgInFrom

# Builds upstream Telegraf with the remp_elasticsearch output plugin added.
# Use the Makefile; it passes TELEGRAF_VERSION, RELEASE and BUILD_TAGS.
#
#   --target binary  static binary only (export with --output type=local)
#   --target image   (default) official telegraf:<version>-alpine image with the binary replaced

ARG TELEGRAF_VERSION
ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TELEGRAF_VERSION
ARG RELEASE
ARG BUILD_TAGS=""
ARG TARGETOS
ARG TARGETARCH
# GOTOOLCHAIN=auto downloads a newer Go if a future Telegraf release needs it.
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto

RUN git clone --quiet --depth 1 --branch "v${TELEGRAF_VERSION}" https://github.com/influxdata/telegraf.git /src
WORKDIR /src

# Add the plugin to the Telegraf tree and register it like the other outputs
# (included in full builds and in custom builds with its build tag).
COPY remp_elasticsearch/*.go remp_elasticsearch/sample.conf plugins/outputs/remp_elasticsearch/
COPY <<'EOF' plugins/outputs/all/remp_elasticsearch.go
//go:build !custom || outputs || outputs.remp_elasticsearch

package all

import _ "github.com/influxdata/telegraf/plugins/outputs/remp_elasticsearch" // register plugin
EOF
# Use the go-elasticsearch version the plugin was tested with (no-op when
# Telegraf pins the same one).
COPY remp_elasticsearch/go.mod /tmp/plugin.go.mod
RUN --mount=type=cache,target=/go/pkg/mod \
    v=$(awk '$1 == "github.com/elastic/go-elasticsearch/v8" { print $2 }' /tmp/plugin.go.mod) && \
    test -n "$v" && go get "github.com/elastic/go-elasticsearch/v8@$v"

# Version stays the upstream one (Telegraf compares it when checking deprecated
# options); the REMP release is recorded as the branch.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    pkg=github.com/influxdata/telegraf/internal && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -tags "${BUILD_TAGS}" \
        -ldflags "-w -s -X ${pkg}.Version=${TELEGRAF_VERSION} -X ${pkg}.Branch=remp-${RELEASE} -X ${pkg}.Commit=$(git rev-parse --short=8 HEAD)" \
        -o /out/telegraf ./cmd/telegraf

FROM scratch AS binary
ARG RELEASE
ARG TARGETOS
ARG TARGETARCH
COPY --from=build /out/telegraf /telegraf-${RELEASE}_${TARGETOS}_${TARGETARCH}

FROM telegraf:${TELEGRAF_VERSION}-alpine AS upstream

# The official image with only /usr/bin/telegraf swapped. Copying the root
# filesystem without the upstream binary keeps the image from carrying both
# binaries (overwriting the file would leave the original in a lower layer).
# The image config below mirrors `docker inspect telegraf:<version>-alpine`.
FROM scratch AS image
ARG TELEGRAF_VERSION
COPY --from=upstream --exclude=usr/bin/telegraf / /
COPY --from=build /out/telegraf /usr/bin/telegraf
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TELEGRAF_VERSION=${TELEGRAF_VERSION}
EXPOSE 8125/udp 8092/udp 8094
ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
CMD ["telegraf"]
