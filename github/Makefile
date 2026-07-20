SHELL := /bin/zsh
.SHELLFLAGS := -ec

PLUGIN_ID := github
BIN_DIR := bin
BINARY := $(BIN_DIR)/github-plugin

.PHONY: build install clean

build:
	@mkdir -p "$(BIN_DIR)"
	@GOOS=darwin GOARCH=arm64 go build -C . -o "$(BIN_DIR)/github-plugin-arm64" ./cmd/github-plugin
	@GOOS=darwin GOARCH=amd64 go build -C . -o "$(BIN_DIR)/github-plugin-amd64" ./cmd/github-plugin
	@lipo -create -output "$(BINARY)" \
		"$(BIN_DIR)/github-plugin-arm64" \
		"$(BIN_DIR)/github-plugin-amd64" 2>/dev/null || \
		cp "$(BIN_DIR)/github-plugin-arm64" "$(BINARY)"
	@rm -f "$(BIN_DIR)/github-plugin-arm64" "$(BIN_DIR)/github-plugin-amd64"

install: build
	@INSTALL_DIR="$${HOME}/.config/smuler/plugins/$(PLUGIN_ID)"; \
	mkdir -p "$${INSTALL_DIR}/bin"; \
	cp "$(BINARY)" "$${INSTALL_DIR}/bin/"; \
	cp "manifest.json" "$${INSTALL_DIR}/"; \
	echo "Installed to $${INSTALL_DIR}"

clean:
	@rm -rf "$(BIN_DIR)"
