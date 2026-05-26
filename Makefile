APP     := capelin-go
BUILD_DIR := build
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

# Define target platforms for 'make dist'. Can be overridden:
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build dist clean test fmt vet lint

.DEFAULT_GOAL := build

all: dist

# Build for the current GOOS/GOARCH into build/capelin-go-<os>-<arch>*
build:
	@out=$(BUILD_DIR)/$(APP)-$(GOOS)-$(GOARCH); \
	if [ "$(GOOS)" = "windows" ]; then out=$${out}.exe; fi; \
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(LDFLAGS) -o $$out .

# Build for all platforms in PLATFORMS into build/capelin-go-<os>-<arch>*
dist:
	@set -e; \
	for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); \
		out=$(BUILD_DIR)/$(APP)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$${out}.exe; fi; \
		echo "Building $$os/$$arch -> $$out"; \
		GOOS=$$os GOARCH=$$arch go build $(LDFLAGS) -o $$out . || exit 1; \
	done

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(APP)
	rm -rf $(BUILD_DIR)
