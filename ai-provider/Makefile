SHELL := /bin/bash
.SHELLFLAGS := -ec

PLUGIN_ID := ai-provider
BIN_DIR := bin
BINARY := $(BIN_DIR)/ai-provider-plugin

.PHONY: build install clean

build:
	@mkdir -p "$(BIN_DIR)"
	@GOOS=darwin GOARCH=arm64 go build -C . -o "$(BIN_DIR)/ai-provider-plugin-arm64" ./cmd/ai-provider-plugin
	@GOOS=darwin GOARCH=amd64 go build -C . -o "$(BIN_DIR)/ai-provider-plugin-amd64" ./cmd/ai-provider-plugin
	@if command -v lipo >/dev/null 2>&1; then \
		lipo -create -output "$(BINARY)" "$(BIN_DIR)/ai-provider-plugin-arm64" "$(BIN_DIR)/ai-provider-plugin-amd64"; \
	elif command -v llvm-lipo >/dev/null 2>&1; then \
		llvm-lipo -create -output "$(BINARY)" "$(BIN_DIR)/ai-provider-plugin-arm64" "$(BIN_DIR)/ai-provider-plugin-amd64"; \
	elif command -v llvm-lipo-18 >/dev/null 2>&1; then \
		llvm-lipo-18 -create -output "$(BINARY)" "$(BIN_DIR)/ai-provider-plugin-arm64" "$(BIN_DIR)/ai-provider-plugin-amd64"; \
	else \
		cp "$(BIN_DIR)/ai-provider-plugin-arm64" "$(BINARY)"; \
	fi
	@rm -f "$(BIN_DIR)/ai-provider-plugin-arm64" "$(BIN_DIR)/ai-provider-plugin-amd64"

install: build
	@INSTALL_DIR="$${HOME}/.config/smuler/plugins/$(PLUGIN_ID)"; \
	mkdir -p "$${INSTALL_DIR}/bin"; \
	cp "$(BINARY)" "$${INSTALL_DIR}/bin/"; \
	cp "manifest.json" "$${INSTALL_DIR}/"; \
	echo "Installed to $${INSTALL_DIR}"

clean:
	@rm -rf "$(BIN_DIR)"
