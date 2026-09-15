# click-dog Makefile.
#
# Layout:
#   Makefile           this file — configuration, the confirm guard, and help
#   Makefile.shared    every target that works in any clone of this project
#   Makefile.internal  targets that only mean something in the development
#                      repository (publishing, the docs site). Export-ignored,
#                      so it is absent from the public tree and the -include
#                      below silently does nothing there.
#
# Targets are grouped by what they do: test- runs tests, check- runs tests plus
# linting and analysis, build- produces artifacts, release- ships something,
# and print- only tells you something. `make help` lists them.

.PHONY: help doctor

include scripts/tool-versions.env

PUBLIC_REPO ?= coltconsulting/click-dog
DOCKER_REPO ?= ghcr.io/coltconsulting/click-dog
DOCKER_TAG  ?= latest
DOCKER_MULTIARCH_OUTPUT ?= type=oci,dest=click-dog-$(DOCKER_TAG).oci.tar
RSVG_CONVERT ?= rsvg-convert
AGENT_CACHE_DIR ?= $(CURDIR)/.agent-cache
LOCAL_TOOLS_BIN := $(AGENT_CACHE_DIR)/bin
GOLANGCI_LINT := $(if $(wildcard $(LOCAL_TOOLS_BIN)/golangci-lint),$(LOCAL_TOOLS_BIN)/golangci-lint,golangci-lint)
GOVULNCHECK := $(if $(wildcard $(LOCAL_TOOLS_BIN)/govulncheck),$(LOCAL_TOOLS_BIN)/govulncheck,govulncheck)
ACTIONLINT := $(if $(wildcard $(LOCAL_TOOLS_BIN)/actionlint),$(LOCAL_TOOLS_BIN)/actionlint,actionlint)
KUBECONFORM := $(if $(wildcard $(LOCAL_TOOLS_BIN)/kubeconform),$(LOCAL_TOOLS_BIN)/kubeconform,kubeconform)
SHELLCHECK := $(if $(wildcard $(LOCAL_TOOLS_BIN)/shellcheck),$(LOCAL_TOOLS_BIN)/shellcheck,shellcheck)
OPENSPEC := $(if $(wildcard $(AGENT_CACHE_DIR)/npm/node_modules/.bin/openspec),$(AGENT_CACHE_DIR)/npm/node_modules/.bin/openspec,openspec)
MKDOCS := $(if $(wildcard $(AGENT_CACHE_DIR)/venv/bin/mkdocs),$(AGENT_CACHE_DIR)/venv/bin/mkdocs,mkdocs)
SHA         := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
YY          := $(shell date +%y)
MM          := $(shell date +%m)

# The version stamped into a local build. Always the next alpha, because a
# working-tree build is a dev build. Release tags are cut by release-alpha /
# release-beta / release-ga, which compute their own tag from the channel —
# there is no channel flag.
#
#   v=YY.MM.idx   pin the version base explicitly (both here and in the
#                 release targets); otherwise it is the next unused GA index.
#
# An extracted source archive has no git metadata, so there are no tags to read
# and next-release-tag.sh refuses. That is the honest answer: a computed tag
# there would claim to be the next alpha of a release line it cannot see. Such
# a build says `dev`, and every other target still works.
ifeq ($(SHA),unknown)
NEXT_TAG    := dev
VERSION     ?= dev
else
NEXT_TAG    := $(shell scripts/next-release-tag.sh alpha "$(v)" "$(YY)" "$(MM)")
ifeq ($(strip $(NEXT_TAG)),)
$(error failed to compute next release tag)
endif
VERSION     ?= $(NEXT_TAG)-$(SHA)
endif

# Guard for unusual, hard-to-undo steps. The target prints what it is about to
# do, then this requires an explicit yes.
#
#   interactive          -> prompts
#   YES=1                -> proceeds without prompting (scripted/unattended use)
#   no TTY and no YES=1  -> fails closed, so CI or a pipe cannot answer for you
#
# Pure shell (no leading @) so it works both as its own recipe line, prefixed
# with @, and spliced into the middle of a continued shell block.
# Only `make … YES=1` counts. A bare non-empty value (YES=0, YES=false), a
# composite one (YES='1 0'), or a YES inherited from the environment must not
# approve an irreversible action. Both tests are exact string comparisons:
# $(filter 1,$(YES)) would match any whitespace-separated word.
YES_CONFIRMED :=
ifeq ($(origin YES),command line)
ifeq ($(YES),1)
YES_CONFIRMED := 1
endif
endif

define confirm_sh
if [ -n "$(YES_CONFIRMED)" ]; then \
		echo "YES=1 given on the command line — continuing without prompting."; \
		echo ""; \
	elif [ ! -t 0 ]; then \
		echo ""; \
		echo "FAIL: $@ needs an interactive terminal to confirm."; \
		echo "      For unattended use, re-run with YES=1 to state intent explicitly."; \
		exit 1; \
	else \
		printf 'Continue? [y/N] '; \
		read -r reply; \
		case "$$reply" in \
			y|Y|yes|YES) echo "" ;; \
			*) echo "aborted."; exit 1 ;; \
		esac; \
	fi
endef

.DEFAULT_GOAL := help

# Print the documented targets (any target carrying a `## ` blurb below).
help: ## Show this help
	@echo "click-dog — common targets (usage: make <target>):"
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# Diagnose repository state, tool versions, and environment constraints without
# changing git or external state.
doctor: ## Diagnose branch/remote, toolchain, caches, and optional capabilities
	bash scripts/doctor.sh

include Makefile.shared
-include Makefile.internal
