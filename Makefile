VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test release clean
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/backpack-plus ./cmd/backpack-plus

test:
	go vet ./...
	go test -race ./...

release:
	mkdir -p dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/backpack-plus-linux-$$arch ./cmd/backpack-plus; \
	done
	cd dist && sha256sum backpack-plus-linux-* > SHA256SUMS

clean:
	rm -rf bin dist
