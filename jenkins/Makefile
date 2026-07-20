SHELL := /bin/zsh
.SHELLFLAGS := -ec

PLUGIN_ID := jenkins
BIN_DIR := bin
BINARY := $(BIN_DIR)/jenkins-plugin

.PHONY: build install clean

build:
	@mkdir -p "$(BIN_DIR)"
	@GOOS=darwin GOARCH=arm64 go build -C . -o "$(BIN_DIR)/jenkins-plugin-arm64" ./cmd/jenkins-plugin
	@GOOS=darwin GOARCH=amd64 go build -C . -o "$(BIN_DIR)/jenkins-plugin-amd64" ./cmd/jenkins-plugin
	@lipo -create -output "$(BINARY)" \
		"$(BIN_DIR)/jenkins-plugin-arm64" \
		"$(BIN_DIR)/jenkins-plugin-amd64" 2>/dev/null || \
		cp "$(BIN_DIR)/jenkins-plugin-arm64" "$(BINARY)"
	@rm -f "$(BIN_DIR)/jenkins-plugin-arm64" "$(BIN_DIR)/jenkins-plugin-amd64"

install: build
	@INSTALL_DIR="$${HOME}/.config/smuler/plugins/$(PLUGIN_ID)"; \
	mkdir -p "$${INSTALL_DIR}/bin"; \
	cp "$(BINARY)" "$${INSTALL_DIR}/bin/"; \
	cp "manifest.json" "$${INSTALL_DIR}/"; \
	echo "Installed to $${INSTALL_DIR}"

clean:
	@rm -rf "$(BIN_DIR)"
