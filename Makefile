SHELL := /bin/bash
BIN := $(CURDIR)/bin
APP := $(CURDIR)/app

.PHONY: all build build-core test vet swift-check app phase0 phase1 phase2 phase3 release clean install-agent uninstall-agent

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

# 更新回滚故障注入：编译真实的 UpdateManager 并在临时目录注入失败
swift-check:
	./scripts/update_rollback_check.sh

app:
	cd app && xcodegen generate
	cd app && xcodebuild -project OrbSleepManager.xcodeproj -scheme OrbSleepManager \
		-configuration Debug -destination 'platform=macOS' SYMROOT="$(APP)/build" build

phase0: build-core
	python3 scripts/phase0.py

phase1: build-core
	python3 scripts/phase1_lifecycle.py

phase2: build-core
	python3 scripts/phase2_docrun.py

phase3: build-core
	python3 scripts/phase3_multienv.py

release:
	./scripts/package-release.sh

install-agent: build-core
	"$(BIN)/orbsleepctl" install-agent --binary "$(BIN)/orbsleepd"

uninstall-agent:
	"$(BIN)/orbsleepctl" uninstall-agent

clean:
	rm -rf "$(BIN)" app/build app/OrbSleepManager.xcodeproj

reliability-e2e: build-core
	python3 scripts/reliability_e2e.py

fault-injection-e2e: build-core
	python3 scripts/fault_injection_e2e.py
