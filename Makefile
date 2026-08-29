BIN      := bedrock-compass
DIST     := dist
# The version var lives in package main (cmd/bedrock-compass/main.go).
# For a main package the linker's -X import path is "main" — NOT the directory
# path. -X github.com/tonymontoya/bedrock-compass/cmd/bedrock-compass.version=
# is silently ignored (builds fine, ships the default). main.version is the
# canonical, portable form.
VERSION_PKG := main

# VERSION: explicit override (make VERSION=v1.0.0 build) > git describe (tag /
# commit) > default. git describe --tags yields e.g. "v1.0.0-3-ga119f7f"; the
# --dirty marker is kept so a working-tree build is never mistaken for a tag.
# `|| true` keeps the build working in a non-git checkout.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS   := -s -w -X $(VERSION_PKG).version=$(VERSION)

.PHONY: check test vet fmt build clean version

check: vet fmt test build

test:
	go test ./...

vet:
	go vet ./...

fmt:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "gofmt needed for:"; echo "$$files"; exit 1; fi

version: ## Print the version that `build` will stamp
	@echo $(VERSION)

build:
	@echo "stamping version: $(VERSION)"
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN) ./cmd/bedrock-compass
	@file $(DIST)/$(BIN) 2>/dev/null | grep -q "statically linked" && echo "static binary: OK" || echo "static binary: could not verify (file(1) missing?)"
	@$(DIST)/$(BIN) --version

clean:
	rm -rf $(DIST)

# --- integration (opt-in: scratch rig on the test host) ---
# RIG_HOST is the ssh target that runs the scratch rig (required).
RIG_HOST ?=

.PHONY: integ build-integ

build-integ: ## Build the static integration test binaries
	mkdir -p .tmp
	CGO_ENABLED=0 go test -c -tags integration -o .tmp/compass-integration.test ./compass/
	CGO_ENABLED=0 go test -c -tags integration -o .tmp/cmd-integration.test ./cmd/bedrock-compass/

integ: build-integ ## Ship and run the integration tests on the rig host
	@test -n "$(RIG_HOST)" || { echo "set RIG_HOST=<ssh target>, e.g. RIG_HOST=rig make integ"; exit 1; }
	scp .tmp/compass-integration.test $(RIG_HOST):/tmp/compass-integration.test
	scp .tmp/cmd-integration.test $(RIG_HOST):/tmp/cmd-integration.test
	$(MAKE) build
	scp dist/bedrock-compass $(RIG_HOST):/tmp/bedrock-compass
	ssh $(RIG_HOST) '/tmp/compass-integration.test -test.v -test.run TestIntegration'
	ssh $(RIG_HOST) '/tmp/cmd-integration.test -test.v -test.run TestIntegration'
