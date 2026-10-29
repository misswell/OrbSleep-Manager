SHELL := /bin/bash
BIN := $(CURDIR)/bin
APP := $(CURDIR)/app

.PHONY: all build build-core test vet app phase0 release clean install-agent uninstall-agent

all: build

build: build-core app

build-core:
	@mkdir -p "$(BIN)"
	cd core && go build -o "$(BIN)/orbsleepd" ./cmd/orbsleepd
	cd core && go build -o "$(BIN)/orbsleepctl" ./cmd/orbsleepctl
	@echo "built $(BIN)/orbsleepd $(BIN)/orbsleepctl"

test:
	cd core && go test ./...

vet:
	cd core && go vet ./...

app:
	cd app && xcodegen generate
	cd app && xcodebuild -project OrbSleepManager.xcodeproj -scheme OrbSleepManager \
		-configuration Debug -destination 'platform=macOS' SYMROOT="$(APP)/build" build

phase0: build-core
	python3 scripts/phase0.py

release:
	./scripts/package-release.sh

install-agent: build-core
	"$(BIN)/orbsleepctl" install-agent --binary "$(BIN)/orbsleepd"

uninstall-agent:
	"$(BIN)/orbsleepctl" uninstall-agent

clean:
	rm -rf "$(BIN)" app/build app/OrbSleepManager.xcodeproj
