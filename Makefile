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
