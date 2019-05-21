ifeq ($(SHELL), cmd)
	VERSION := $(shell git describe --exact-match --tags 2>nil)
	HOME := $(HOMEPATH)
else ifeq ($(SHELL), sh.exe)
	VERSION := $(shell git describe --exact-match --tags 2>nil)
	HOME := $(HOMEPATH)
else
	VERSION := $(shell git describe --exact-match --tags 2>/dev/null)
endif

PREFIX := /usr/local
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)
COMMIT := $(shell git rev-parse --short HEAD)
GOFILES ?= $(shell git ls-files '*.go')
GOFMT ?= $(shell gofmt -l -s $(filter-out plugins/parsers/influx/machine.go, $(GOFILES)))
GOPATH ?= $(shell go env GOPATH)
IMAGEVER ?= latest
BUILDFLAGS ?=

ifdef GOBIN
PATH := $(GOBIN):$(PATH)
else
PATH := $(subst :,/bin:,$(shell go env GOPATH))/bin:$(PATH)
endif

LDFLAGS := $(LDFLAGS) -X main.commit=$(COMMIT) -X main.branch=$(BRANCH)
ifdef VERSION
	LDFLAGS += -X main.version=$(VERSION)
endif

.PHONY: all
all:
	@$(MAKE) --no-print-directory deps
	@$(MAKE) --no-print-directory telegraf

.PHONY: deps
deps:
	dep ensure -vendor-only

.PHONY: telegraf
telegraf:
	go build -ldflags "$(LDFLAGS)" ./cmd/telegraf

.PHONY: go-install
go-install:
	go install -ldflags "-w -s $(LDFLAGS)" ./cmd/telegraf

.PHONY: install
install: telegraf
	mkdir -p $(DESTDIR)$(PREFIX)/bin/
	cp telegraf $(DESTDIR)$(PREFIX)/bin/


.PHONY: test
test:
	go test -short ./...

.PHONY: fmt
fmt:
	@gofmt -s -w $(filter-out plugins/parsers/influx/machine.go, $(GOFILES))

.PHONY: fmtcheck
fmtcheck:
	@if [ ! -z "$(GOFMT)" ]; then \
		echo "[ERROR] gofmt has found errors in the following files:"  ; \
		echo "$(GOFMT)" ; \
		echo "" ;\
		echo "Run make fmt to fix them." ; \
		exit 1 ;\
	fi

.PHONY: test-windows
test-windows:
	go test -short ./plugins/inputs/ping/...
	go test -short ./plugins/inputs/win_perf_counters/...
	go test -short ./plugins/inputs/win_services/...
	go test -short ./plugins/inputs/procstat/...
	go test -short ./plugins/inputs/ntpq/...

.PHONY: vet
vet:
	@echo 'go vet $$(go list ./... | grep -v ./plugins/parsers/influx)'
	@go vet $$(go list ./... | grep -v ./plugins/parsers/influx) ; if [ $$? -ne 0 ]; then \
		echo ""; \
		echo "go vet has found suspicious constructs. Please remediate any reported errors"; \
		echo "to fix them before submitting code for review."; \
		exit 1; \
	fi

.PHONY: check
check: fmtcheck vet

.PHONY: test-all
test-all: fmtcheck vet
	go test ./...

.PHONY: package
package:
	./scripts/build.py --package --platform=all --arch=all

.PHONY: package-release
package-release:
	./scripts/build.py --release --package --platform=all --arch=all \
		--upload --bucket=dl.influxdata.com/telegraf/releases

.PHONY: package-nightly
package-nightly:
	./scripts/build.py --nightly --package --platform=all --arch=all \
		--upload --bucket=dl.influxdata.com/telegraf/nightlies

.PHONY: clean
clean:
	rm -f telegraf
	rm -f telegraf.exe

.PHONY: docker-image
docker-image:
	docker build -f scripts/stretch.docker -t "telegraf:$(COMMIT)" .

docker-telegraf:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o telegraf -installsuffix cgo -i -ldflags "$(LDFLAGS)" ./cmd/telegraf

docker-build:
	set -e; \
	docker build -t remp-telegraf_builder ./docker_builder; \
	docker run --rm -v $$PWD:/src/build -v ${GOPATH}/src:/gopath/src remp-telegraf_builder > ./docker_runner/telegraf.tar;

docker-push:
	set -e; \
	docker login
	docker build -t remp-telegraf_builder ./docker_builder; \
	docker run --rm -v $$PWD:/src/build -v ${GOPATH}/src:/gopath/src remp-telegraf_builder > ./docker_runner/telegraf.tar;
	docker build -t remp/telegraf:${IMAGEVER} ./docker_runner; \
	docker push remp/telegraf:${IMAGEVER}

plugins/parsers/influx/machine.go: plugins/parsers/influx/machine.go.rl
	ragel -Z -G2 $^ -o $@

.PHONY: static
static:
	@echo "Building static linux binary..."
	@CGO_ENABLED=0 \
	GOOS=linux \
	GOARCH=amd64 \
	go build -ldflags "$(LDFLAGS)" ./cmd/telegraf

.PHONY: plugin-%
plugin-%:
	@echo "Starting dev environment for $${$(@)} input plugin..."
	@docker-compose -f plugins/inputs/$${$(@)}/dev/docker-compose.yml up

.PHONY: ci-1.11
ci-1.11:
	docker build -t quay.io/influxdb/telegraf-ci:1.11.5 - < scripts/ci-1.11.docker
	docker push quay.io/influxdb/telegraf-ci:1.11.5

.PHONY: ci-1.10
ci-1.10:
	docker build -t quay.io/influxdb/telegraf-ci:1.10.8 - < scripts/ci-1.10.docker
	docker push quay.io/influxdb/telegraf-ci:1.10.8

.PHONY: ci-1.9
ci-1.9:
	docker build -t quay.io/influxdb/telegraf-ci:1.9.7 - < scripts/ci-1.9.docker
	docker push quay.io/influxdb/telegraf-ci:1.9.7
