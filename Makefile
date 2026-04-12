APP := spinherd
STATIC_APP := $(APP)-static
BUILD_DIR := $(CURDIR)/build
BIN_ROOT_DIR := $(BUILD_DIR)/bin
HOST_BIN_DIR := $(BIN_ROOT_DIR)/host
HOST_BIN := $(HOST_BIN_DIR)/$(APP)
STATIC_BIN := $(HOST_BIN_DIR)/$(STATIC_APP)
GO_SOURCES := $(wildcard *.go)
GOCACHE := $(BUILD_DIR)/gocache
GOMODCACHE := $(BUILD_DIR)/gomodcache
GOPATH := $(BUILD_DIR)/gopath
GOTMPDIR := $(BUILD_DIR)/tmp
GOTELEMETRYDIR := $(BUILD_DIR)/telemetry
GOENV := off
GOFLAGS := -modcacherw
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

export GOCACHE
export GOMODCACHE
export GOPATH
export GOTMPDIR
export GOTELEMETRYDIR
export GOENV
export GOFLAGS
export GOTELEMETRY=off

.PHONY: all build static test run install clean

all: build

build: $(HOST_BIN)

static: $(STATIC_BIN)

test:
	mkdir -p "$(BUILD_DIR)" "$(GOCACHE)" "$(GOMODCACHE)" "$(GOPATH)" "$(GOTMPDIR)" "$(GOTELEMETRYDIR)"
	go test ./...

$(HOST_BIN): go.mod $(GO_SOURCES)
	mkdir -p "$(HOST_BIN_DIR)" "$(GOCACHE)" "$(GOMODCACHE)" "$(GOPATH)" "$(GOTMPDIR)" "$(GOTELEMETRYDIR)"
	go build -o "$(HOST_BIN)" .

$(STATIC_BIN): go.mod $(GO_SOURCES)
	mkdir -p "$(HOST_BIN_DIR)" "$(GOCACHE)" "$(GOMODCACHE)" "$(GOPATH)" "$(GOTMPDIR)" "$(GOTELEMETRYDIR)"
	CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(STATIC_BIN)" .

run: build
	"$(HOST_BIN)"

install: build
	if [ "$$(id -u)" -eq 0 ]; then \
		install -m 0755 "$(HOST_BIN)" "/usr/local/bin/$(APP)"; \
	else \
		mkdir -p "$$HOME/.local/bin"; \
		ln -sfn "$(HOST_BIN)" "$$HOME/.local/bin/$(APP)"; \
	fi

clean:
	chmod -R u+w "$(BUILD_DIR)" 2>/dev/null || true
	rm -rf "$(BUILD_DIR)"
