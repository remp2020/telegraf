# Telegraf for REMP

Upstream [Telegraf](https://github.com/influxdata/telegraf) with the
[`remp_elasticsearch`](remp_elasticsearch/README.md) output plugin, which
writes REMP Beam events to Elasticsearch.

This repository is not a Telegraf fork. It contains only the plugin
(`remp_elasticsearch/`, a standalone Go module with its tests); the
`Dockerfile` clones the pinned upstream release, adds the plugin and compiles
Telegraf. Upgrading Telegraf means changing one version, not merging upstream.

The module path `github.com/influxdata/telegraf/plugins/outputs/remp_elasticsearch`
is the plugin's package path inside the Telegraf tree. That lets it use
`github.com/influxdata/telegraf/internal` (to report partially failed writes),
which Go allows only under `github.com/influxdata/telegraf`. The module is
never published.

## Requirements

- Docker 23 or newer (builds run in containers with plain `docker build`)
- Go (see `remp_elasticsearch/go.mod`) for `make test` and `make upgrade`

## Usage

```sh
make test               # unit tests
make test-integration   # + tests against Elasticsearch 8.6.1 in Docker

make binary             # static binary -> dist/telegraf-1.40.3-1_linux_amd64
make docker-build       # image remp/telegraf:1.40.3-1 in the local Docker
make docker-push        # build and push it; refuses to overwrite an existing tag (FORCE=1)
make release            # full + slim .tar.gz for linux/amd64 and linux/arm64 + SHA256SUMS

make docker-push FLAVOR=slim                           # remp/telegraf:1.40.3-1-slim
make docker-push PLATFORMS="linux/amd64 linux/arm64"   # multi-arch image
make docker-push REVISION=2                            # plugin fix on the same Telegraf
```

`make help` lists all targets and variables. `FLAVOR=slim` builds only the
plugins in `SLIM_PLUGINS` (Kafka and Pub/Sub inputs, influx parser, this
output). A multi-platform `docker-push` needs Docker's containerd image store
(default on new installs since Docker 29).

The image is the official `telegraf:<version>-alpine` (same packages, user,
entrypoint) with only the binary replaced, so it's a drop-in for the previous
`remp/telegraf` images. `make docker-build` fails if a new upstream image
changes its configuration. Sizes: full about 460 MB, slim about 95 MB.
`telegraf --version` prints e.g. `Telegraf 1.40.3 (git: remp-1.40.3-1@41b45388)`.

## Upgrading Telegraf

```sh
make upgrade TO=1.41.0    # bumps remp_elasticsearch/go.mod, runs lint and unit tests
make test-integration
make release              # REVISION defaults to 1 -> 1.41.0-1
make docker-push
```

If the build fails after an upgrade, it's a compile error in the plugin caused
by a Telegraf API change. Also read the upstream changelog for changed agent
defaults.

Releases up to 1.35.3 were built from the `remp` branch, a fork of Telegraf
that merged upstream releases; it's kept for history.
