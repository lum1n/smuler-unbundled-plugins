SHELL := /bin/zsh
.SHELLFLAGS := -ec

PLUGIN_ID := agent-monitor
BIN_DIR := bin
BINARY := $(BIN_DIR)/agent-monitor-plugin

.PHONY: build install clean

build:
	@mkdir -p "$(BIN_DIR)"
	@GOOS=darwin GOARCH=arm64 go build -C . -o "$(BIN_DIR)/agent-monitor-plugin-arm64" ./cmd/agent-monitor-plugin
	@GOOS=darwin GOARCH=amd64 go build -C . -o "$(BIN_DIR)/agent-monitor-plugin-amd64" ./cmd/agent-monitor-plugin
	@lipo -create -output "$(BINARY)" \
		"$(BIN_DIR)/agent-monitor-plugin-arm64" \
		"$(BIN_DIR)/agent-monitor-plugin-amd64" 2>/dev/null || \
		cp "$(BIN_DIR)/agent-monitor-plugin-arm64" "$(BINARY)"
	@rm -f "$(BIN_DIR)/agent-monitor-plugin-arm64" "$(BIN_DIR)/agent-monitor-plugin-amd64"

install: build
	@INSTALL_DIR="$${HOME}/.config/smuler/plugins/$(PLUGIN_ID)"; \
	mkdir -p "$${INSTALL_DIR}/bin"; \
	cp "$(BINARY)" "$${INSTALL_DIR}/bin/"; \
	cp "manifest.json" "$${INSTALL_DIR}/"; \
	if [ -d "hooks" ]; then cp -R "hooks" "$${INSTALL_DIR}/"; fi; \
	if [ -d "third_party/agent-watcher/src" ]; then \
		mkdir -p "$${INSTALL_DIR}/watcher"; \
		cp third_party/agent-watcher/src/agent_watcher.py "$${INSTALL_DIR}/watcher/"; \
		if [ -d "third_party/agent-watcher/src/harnesses" ]; then \
			cp -R third_party/agent-watcher/src/harnesses "$${INSTALL_DIR}/watcher/"; \
		fi; \
	fi; \
	echo "Installed to $${INSTALL_DIR}"

clean:
	@rm -rf "$(BIN_DIR)"
