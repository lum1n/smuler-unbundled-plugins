SHELL := /bin/zsh
.SHELLFLAGS := -ec

PLUGIN_ID := cursor-cloud-agents
BIN_DIR := bin
BINARY := $(BIN_DIR)/cursor-cloud-agents-plugin

.PHONY: build install clean test

build:
	@mkdir -p "$(BIN_DIR)"
	@GOOS=darwin GOARCH=arm64 go build -C . -o "$(BIN_DIR)/cursor-cloud-agents-plugin-arm64" ./cmd/cursor-cloud-agents-plugin
	@GOOS=darwin GOARCH=amd64 go build -C . -o "$(BIN_DIR)/cursor-cloud-agents-plugin-amd64" ./cmd/cursor-cloud-agents-plugin
	@lipo -create -output "$(BINARY)" \
		"$(BIN_DIR)/cursor-cloud-agents-plugin-arm64" \
		"$(BIN_DIR)/cursor-cloud-agents-plugin-amd64" 2>/dev/null || \
		cp "$(BIN_DIR)/cursor-cloud-agents-plugin-arm64" "$(BINARY)"
	@rm -f "$(BIN_DIR)/cursor-cloud-agents-plugin-arm64" "$(BIN_DIR)/cursor-cloud-agents-plugin-amd64"

install: build
	@INSTALL_DIR="$${HOME}/.config/smuler/plugins/$(PLUGIN_ID)"; \
	mkdir -p "$${INSTALL_DIR}/bin"; \
	cp "$(BINARY)" "$${INSTALL_DIR}/bin/"; \
	cp "manifest.json" "$${INSTALL_DIR}/"; \
	echo "Installed to $${INSTALL_DIR}"

test:
	@go test ./...

clean:
	@rm -rf "$(BIN_DIR)"
