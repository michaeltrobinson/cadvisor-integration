ALL_DIRS=$(shell find . \( -path ./Godeps -o -path ./vendor -o -path ./.git \) -prune -o -type d -print)
GO_PKGS=$(foreach pkg, $(shell go list ./...), $(if $(findstring /vendor/, $(pkg)), , $(pkg)))
EXECUTABLE=signalfx-cadvisord
CMD_DIR=cmd/$(EXECUTABLE)
DOCKER_DIR=Docker
DOCKER_FILE=$(DOCKER_DIR)/Dockerfile
GO_FILES=$(foreach dir, $(ALL_DIRS), $(wildcard $(dir)/*.go))
PKG=$(filter %$(CMD_DIR), $(GO_PKGS))
DOCKER_RELEASE_TAG=$(shell date +%Y%m%d-%H%M%S)
SRC_DIR = $(shell cd ../../.. && pwd)
METALINT = "gometalinter --cyclo-over=10 -D gotype -t"
GOLANG_VERSION ?= 1.5.1
GOTEST ?= go test -v # use "gt" if you want results cached for quicker testing
GO_LINTERS ?= golint "go vet"
DOCKER_IMAGE=michaeltrobinson/$(EXECUTABLE)

GIT_COMMIT=unknown

ifeq ("$(CIRCLECI)", "true")
	CI_SERVICE = circle-ci
	export GIT_BRANCH = $(CIRCLE_BRANCH)
	GIT_COMMIT = $(CIRCLE_SHA1)
endif

export GO15VENDOREXPERIMENT=1

all: build

lint:
	@for pkg in $(GO_PKGS); do \
		for cmd in $(GO_LINTERS); do \
			eval "$$cmd $$pkg" | grep -v '\/rules\.pb\.go:' | grep -v '\/bindata\.go:' || true; \
		done; \
	done

metalint:
	@for pkg in $(GO_PKGS); do \
		eval "$(METALINT) $(SRC_DIR)/$$pkg" | grep -v '\/rules\.pb\.go:' | grep -v '\/bindata\.go:' || true; \
	done

test: $(GO_FILES)
	@for pkg in $(GO_PKGS); do \
		if test $$pkg = $(PKG); then continue; fi; \
		cmd="$(GOTEST) -race $$pkg"; \
		eval $$cmd; \
		if test $$? -ne 0; then \
			exit 1; \
		fi; \
	done

coverage: .acc.out

.acc.out: $(GO_FILES)
	@echo "mode: set" > .acc.out
	@for pkg in $(GO_PKGS); do \
		if test $$pkg = $(PKG); then continue; fi; \
		if test $$pkg = $(DUMP_PKG); then continue; fi; \
		cmd="$(GOTEST) -coverprofile=profile.out $$pkg"; \
		eval $$cmd; \
		if test $$? -ne 0; then \
			exit 1; \
		fi; \
		if test -f profile.out; then \
			cat profile.out | grep -v "mode: set" >> .acc.out; \
		fi; \
	done
	@rm -f ./profile.out

coveralls: .coveralls-stamp

.coveralls-stamp: .acc.out
	@if [ -n "$(COVERALLS_REPO_TOKEN)" ]; then \
		goveralls -v -coverprofile=.acc.out -service $(CI_SERVICE) -repotoken $(COVERALLS_REPO_TOKEN); \
	fi
	@touch .coveralls-stamp

build: $(EXECUTABLE)

$(EXECUTABLE): $(GO_FILES)
	go build -v -o $(EXECUTABLE) $(PKG)

clean:
	@rm -f ./.acc.out \
      $(EXECUTABLE) \
      $(DOCKER_DIR)/$(EXECUTABLE) \
      ./.image-stamp \
      ./.save-stamp \
      ./.coveralls-stamp

save: .save-stamp

.save-stamp: $(GO_FILES)
	@rm -rf ./Godeps ./vendor
	GOOS=linux GOARCH=amd64 godep save ./...
	@touch .save-stamp

$(DOCKER_DIR)/$(EXECUTABLE): $(GO_FILES)
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -v -tags netgo -installsuffix netgo -o $(DOCKER_DIR)/$(EXECUTABLE) $(PKG)

image: .image-stamp

.image-stamp: $(DOCKER_DIR)/$(EXECUTABLE) $(DOCKER_FILE)
	@sed -i.bak '/^ENV GIT_COMMIT/d' $(DOCKER_FILE)
	@rm -f $(DOCKER_FILE).bak
	@echo "ENV GIT_COMMIT $(GIT_COMMIT)" >> $(DOCKER_FILE)
	docker build -t $(DOCKER_IMAGE) $(DOCKER_DIR)
	@touch .image-stamp

push:
	docker tag -f $(DOCKER_IMAGE):latest $(DOCKER_IMAGE):$(DOCKER_RELEASE_TAG)
	docker push $(DOCKER_IMAGE):latest
	docker push $(DOCKER_IMAGE):$(DOCKER_RELEASE_TAG)

$(HOME)/go/go$(GOLANG_VERSION).linux-amd64.tar.gz:
	@mkdir -p $(HOME)/go
	wget https://storage.googleapis.com/golang/go$(GOLANG_VERSION).linux-amd64.tar.gz -O $(HOME)/go/go$(GOLANG_VERSION).linux-amd64.tar.gz

$(HOME)/go/go$(GOLANG_VERSION)/bin/go: $(HOME)/go/go$(GOLANG_VERSION).linux-amd64.tar.gz
	@tar -C $(HOME)/go -zxf $(HOME)/go/go$(GOLANG_VERSION).linux-amd64.tar.gz
	@mv $(HOME)/go/go $(HOME)/go/go$(GOLANG_VERSION)
	@touch $(HOME)/go/go$(GOLANG_VERSION)/bin/go

install_go: $(HOME)/go/go$(GOLANG_VERSION)/bin/go

.PHONY: all lint test coverage coveralls build clean save push install_go
