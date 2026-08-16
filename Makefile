BIN      := bedrock-compass
DIST     := dist
LDFLAGS  := -s -w

.PHONY: check test vet fmt build clean

check: vet fmt test build

test:
	go test ./...

vet:
	go vet ./...

fmt:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "gofmt needed for:"; echo "$$files"; exit 1; fi

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN) ./cmd/bedrock-compass
	@file $(DIST)/$(BIN) 2>/dev/null | grep -q "statically linked" && echo "static binary: OK" || echo "static binary: could not verify (file(1) missing?)"

clean:
	rm -rf $(DIST)

# --- integration (opt-in: scratch rig on the test host) ---
# RIG_HOST is the ssh target that runs the scratch rig (required).
RIG_HOST ?=

.PHONY: integ build-integ

build-integ: ## Build the static integration test binary
	mkdir -p .tmp
	CGO_ENABLED=0 go test -c -tags integration -o .tmp/compass-integration.test ./compass/

integ: build-integ ## Ship and run the integration tests on the rig host
	@test -n "$(RIG_HOST)" || { echo "set RIG_HOST=<ssh target>, e.g. RIG_HOST=rig make integ"; exit 1; }
	scp .tmp/compass-integration.test $(RIG_HOST):/tmp/compass-integration.test
	ssh $(RIG_HOST) '/tmp/compass-integration.test -test.v -test.run TestIntegration'
