APP     := capelin-go
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build clean test fmt vet lint

all: build

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(LDFLAGS) -o $(APP) .

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(APP)
