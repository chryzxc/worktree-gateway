VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/chryzxc/worktree-gateway/internal/version.Version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

.PHONY: build install test vet lint dist clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/wtg ./cmd/wtg

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/wtg

test:
	go test -race ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

lint: vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

# Cross-compiled release archives + checksums in dist/.
dist: clean
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=wtg_$(VERSION)_$${os}_$${arch}; \
		echo "building $$name"; \
		mkdir -p dist/$$name && \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$name/wtg ./cmd/wtg && \
		cp README.md LICENSE dist/$$name/ && \
		tar -C dist -czf dist/$$name.tar.gz $$name && rm -rf dist/$$name || exit 1; \
	done
	@cd dist && shasum -a 256 *.tar.gz > checksums.txt

clean:
	rm -rf bin dist
