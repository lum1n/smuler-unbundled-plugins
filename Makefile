SHELL := /bin/zsh
.SHELLFLAGS := -ec

# Bundled with the host app: git, vitals, ports, calendar (host-native), terminal-companion.
# Everything else under plugins/ is registry/local-install (see plugins/<id>/Makefile).
REGISTRY_PLUGINS := github linear jira bitbucket confluence ai-provider agent-monitor ci-github-actions email jenkins teams cursor-cloud-agents

.PHONY: help doctor test git-build plugin-check terminal-companion-build ports-build vitals-build plugins-build registry-plugins-build registry-plugins-install registry-plugins-test registry-plugins-publish host-build host-run host-icon host-version host-app host-clean mlx-metallib cli-build cli-install release release-check-signing release-zip release-notarize release-staple release-dmg release-verify release-setup-notary

# Helpers used by plugins (not published).
HELPER_MODULES := sdk-go plugindebug httphealth tools/package-plugin

registry-plugins-test:
	@python3 "$(CURDIR)/scripts/unpublished-plugins.py" --self-test
	@for dir in $(HELPER_MODULES); do \
		echo "Testing $$dir"; \
		GOTOOLCHAIN=local go test -C "./$$dir" ./...; \
	done
	@for plugin in $(REGISTRY_PLUGINS); do \
		echo "Testing $$plugin"; \
		GOTOOLCHAIN=local go test -C "./$$plugin" ./...; \
	done

registry-plugins-build:
	@for plugin in $(REGISTRY_PLUGINS); do \
		echo "Building registry plugin: $$plugin"; \
		$(MAKE) -C "./$$plugin" build; \
	done

registry-plugins-install:
	@for plugin in $(REGISTRY_PLUGINS); do \
		echo "Installing registry plugin: $$plugin"; \
		$(MAKE) -C "./$$plugin" install; \
	done

# Publish unbundled plugins to GitHub releases + smuler-registry.
# Examples:
#   make registry-plugins-publish PUBLISH_ARGS='--check'
#   make registry-plugins-publish PUBLISH_ARGS='--release --tag plugins-v0.1.0'
#   make registry-plugins-publish PUBLISH_ARGS='--release --submit --tag plugins-v0.1.0'
#   make registry-plugins-publish PUBLISH_ARGS='--unpublished-only --release --submit'
PUBLISH_ARGS ?=
registry-plugins-publish:
	@chmod +x "$(CURDIR)/publish-registry-plugins.sh"
	@"$(CURDIR)/publish-registry-plugins.sh" $(PUBLISH_ARGS)

