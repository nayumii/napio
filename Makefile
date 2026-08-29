VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint check integration docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o napio ./cmd/napio

test:
	go test ./... -count=1

# Requires a real Factorio container; see CONTRIBUTING.md.
integration:
	go test ./internal/integration/ -count=1 -v -timeout 10m

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run gofmt -w ." && exit 1)
	go vet ./...

check: lint test

docker:
	docker build --build-arg VERSION=$(VERSION) -t napio:$(VERSION) -t napio:latest .

clean:
	rm -f napio coverage.out
