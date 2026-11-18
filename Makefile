# Telegraf with the REMP Elasticsearch output plugin.
#
# The Telegraf version is the one the plugin module depends on
# (remp_elasticsearch/go.mod); change it with `make upgrade`.

SHELL := /bin/bash

TELEGRAF_VERSION := $(shell awk '$$1 == "github.com/influxdata/telegraf" { sub(/^v/, "", $$2); print $$2 }' remp_elasticsearch/go.mod)
# Bump when the plugin changes without a Telegraf upgrade; reset to 1 on upgrade.
REVISION ?= 1
IMAGEVER ?= $(TELEGRAF_VERSION)-$(REVISION)
IMAGE ?= remp/telegraf

# full: all Telegraf plugins (default, what other REMP users get)
# slim: only the plugins listed in SLIM_PLUGINS
FLAVOR ?= full
SLIM_PLUGINS ?= inputs.kafka_consumer inputs.cloud_pubsub parsers.influx outputs.remp_elasticsearch

PLATFORMS ?= linux/amd64
RELEASE_PLATFORMS ?= linux/amd64 linux/arm64
DIST ?= dist

comma := ,
empty :=
space := $(empty) $(empty)

ifeq ($(FLAVOR),full)
BUILD_TAGS :=
SUFFIX :=
else ifeq ($(FLAVOR),slim)
BUILD_TAGS := custom,$(subst $(space),$(comma),$(strip $(SLIM_PLUGINS)))
SUFFIX := -slim
else
$(error FLAVOR must be "full" or "slim")
endif

# Plain `docker build`; needs Docker 23+ (BuildKit, the default builder since 23.0).
DOCKER_BUILD = docker build \
	--build-arg TELEGRAF_VERSION=$(TELEGRAF_VERSION) \
	--build-arg RELEASE=$(IMAGEVER)$(SUFFIX) \
	--build-arg BUILD_TAGS=$(BUILD_TAGS)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "Telegraf $(TELEGRAF_VERSION), release $(IMAGEVER), flavor $(FLAVOR)"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN { FS = ":.*## " } { printf "  %-18s %s\n", $$1, $$2 }'
	@echo
	@echo "Variables: FLAVOR=full|slim REVISION=N IMAGEVER=x.y.z-N IMAGE=$(IMAGE) PLATFORMS=\"$(PLATFORMS)\""

.PHONY: test
test: ## Run unit tests
	cd remp_elasticsearch && go test -short -count=1 ./...

.PHONY: test-integration
test-integration: ## Run all tests including the ones against an Elasticsearch container (needs Docker)
	cd remp_elasticsearch && go test -count=1 ./...

.PHONY: lint
lint: ## Run gofmt and go vet
	cd remp_elasticsearch && test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	cd remp_elasticsearch && go vet ./...

.PHONY: binary
binary: ## Build static binaries for PLATFORMS into dist/
	@for platform in $(PLATFORMS); do \
		echo "==> $(DIST)/telegraf-$(IMAGEVER)$(SUFFIX)_$${platform%/*}_$${platform#*/}"; \
		$(DOCKER_BUILD) --target binary --platform $$platform --output type=local,dest=$(DIST) . || exit 1; \
	done

.PHONY: release
release: ## Build full and slim binaries for RELEASE_PLATFORMS as .tar.gz with checksums
	rm -rf $(DIST)
	$(MAKE) binary FLAVOR=full PLATFORMS="$(RELEASE_PLATFORMS)"
	$(MAKE) binary FLAVOR=slim PLATFORMS="$(RELEASE_PLATFORMS)"
	@# Each archive contains just "telegraf", like the releases up to 1.35.3.
	cd $(DIST) && for f in telegraf-*; do tar -czf $$f.tar.gz --transform 's|.*|telegraf|' $$f; done && sha256sum *.tar.gz > SHA256SUMS
	@echo && ls -lh $(DIST)/*.tar.gz && echo "Upload $(DIST)/*.tar.gz and $(DIST)/SHA256SUMS to the GitHub release $(IMAGEVER)."

IMAGE_REF = $(IMAGE):$(IMAGEVER)$(SUFFIX)
UPSTREAM_IMAGE = telegraf:$(TELEGRAF_VERSION)-alpine
IMAGE_CONFIG = {{json .Config.Entrypoint}} {{json .Config.Cmd}} {{json .Config.User}} {{json .Config.WorkingDir}} {{json .Config.ExposedPorts}} {{json .Config.Env}} {{json .Config.Labels}}

.PHONY: docker-build
docker-build: ## Build the image into the local Docker (first of PLATFORMS)
	$(DOCKER_BUILD) --target image --platform $(firstword $(PLATFORMS)) -t $(IMAGE_REF) .
	@# The image re-declares the upstream image config (see Dockerfile); make
	@# sure a new Telegraf release didn't change it.
	@docker pull -q --platform $(firstword $(PLATFORMS)) $(UPSTREAM_IMAGE) >/dev/null
	@test "$$(docker inspect -f '$(IMAGE_CONFIG)' $(UPSTREAM_IMAGE))" = "$$(docker inspect -f '$(IMAGE_CONFIG)' $(IMAGE_REF))" || { \
		echo "Image config differs from $(UPSTREAM_IMAGE); update the image stage in the Dockerfile:"; \
		docker inspect -f '$(IMAGE_CONFIG)' $(UPSTREAM_IMAGE) $(IMAGE_REF); exit 1; }

.PHONY: docker-push
docker-push: ## Build the image for PLATFORMS and push it (FORCE=1 overwrites an existing tag)
	docker login
	@if [ -z "$(FORCE)" ] && docker manifest inspect $(IMAGE_REF) >/dev/null 2>&1; then \
		echo "$(IMAGE_REF) already exists; bump REVISION or set FORCE=1"; exit 1; fi
	$(MAKE) docker-build
ifeq ($(words $(PLATFORMS)),1)
	docker push $(IMAGE_REF)
else
	@# A multi-platform image is pushed directly; this needs Docker's containerd
	@# image store (default on new installs since Docker 29).
	$(DOCKER_BUILD) --target image --platform $(subst $(space),$(comma),$(strip $(PLATFORMS))) --push -t $(IMAGE_REF) .
endif

.PHONY: upgrade
upgrade: ## Upgrade Telegraf: make upgrade TO=1.41.0
	@test -n "$(TO)" || { echo "usage: make upgrade TO=<telegraf version>"; exit 1; }
	cd remp_elasticsearch && go get github.com/influxdata/telegraf@v$(TO) && go mod tidy
	$(MAKE) lint test
	@echo && echo "Telegraf $(TO) is in place. Run 'make test-integration', then build with REVISION=1 (release $(TO)-1)."

.PHONY: clean
clean: ## Remove build output
	rm -rf $(DIST)
