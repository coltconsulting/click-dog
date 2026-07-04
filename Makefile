.PHONY: help test build build-all build-amd64 build-arm64 clean run \
       test-coverage docker docker-multiarch docker-multiarch-push docker-push \
       preflight tag release version fmt fmt-check lint deps dev-setup vulncheck \
       integration-up integration-test integration-down integration \
       integration-degradation \
       k8s-validate docs-social-card update-public

DOCKER_REPO ?= ghcr.io/coltconsulting/click-dog
DOCKER_TAG  ?= latest
DOCKER_MULTIARCH_OUTPUT ?= type=oci,dest=click-dog-$(DOCKER_TAG).oci.tar
RSVG_CONVERT ?= rsvg-convert
SHA         := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
YY          := $(shell date +%y)
MM          := $(shell date +%m)
# Count existing tags for this month to derive the next index. The glob
# matches qualified tags too (e.g. v26.05.1-alpha), so prereleases bump
# the idx the same way GA tags do.
TAG_IDX     := $(shell echo $$(( $(shell git tag -l "v$(YY).$(MM).*" 2>/dev/null | wc -l | tr -d ' ') + 1 )))

# Optional release qualifier: test | alpha | beta | ga
# Usage: make tag q=alpha           → vYY.MM.idx-alpha
#        make tag q=ga              → vYY.MM.idx (suffix dropped — GA is the plain tag)
#        make tag v=26.05.1 q=beta  → v26.05.1-beta (explicit version override)
# Anything with a -alpha/-beta/-test suffix is marked prerelease by
# GoReleaser, which gates the docker :latest tag and the GitHub
# "latest release" pointer (see .goreleaser.yaml release.prerelease).
# Validate at parse time so even `make version q=bogus` fails fast.
ifneq ($(q),)
ifeq ($(filter $(q),test alpha beta ga),)
$(error invalid qualifier '$(q)' — must be one of: test, alpha, beta, ga)
endif
endif
QUAL_SUFFIX := $(if $(filter-out ga,$(q)),-$(q),)
NEXT_TAG    := v$(YY).$(MM).$(TAG_IDX)$(QUAL_SUFFIX)
VERSION     ?= $(NEXT_TAG)-$(SHA)

# Show the computed next version
version: ## Print the computed next release version
	@echo "$(VERSION)"

# ── Help ────────────────────────────────────────────────────────────

.DEFAULT_GOAL := help

# Print the documented targets (any target carrying a `## ` blurb below).
help: ## Show this help
	@echo "click-dog — common targets (usage: make <target>):"
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Build ───────────────────────────────────────────────────────────

# Build the binary with version injected
build: ## Build the binary for the current platform (version injected)
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o click-dog

# Build for all release platforms (linux only)
build-all: build-amd64 build-arm64 ## Build linux amd64 + arm64 release binaries

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" -o click-dog-linux-amd64

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w -X main.version=$(VERSION)" -o click-dog-linux-arm64

# ── Test ────────────────────────────────────────────────────────────

test: ## Run unit tests + the install.sh test suite
	go test -v ./...
	bash deploy/install_test.sh

test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

test-race: ## Run unit tests with the race detector
	go test -v -race ./...

test-otel:
	go test -v -run TestOTEL

test-mock-integration:
	go test -v -run Integration

# ── Integration (Real Services) ─────────────────────────────────────

# Start the 3-node ClickHouse cluster + OTEL collector
integration-up:
	docker compose -f testing/docker-compose.integration.yml up -d
	@echo "Waiting for services..."
	@for i in $$(seq 1 60); do \
		docker exec clickhouse-int-1 clickhouse-client -q "SELECT 1" 2>/dev/null && \
		docker exec clickhouse-int-2 clickhouse-client -q "SELECT 1" 2>/dev/null && \
		docker exec clickhouse-int-3 clickhouse-client -q "SELECT 1" 2>/dev/null && \
		break; \
		sleep 2; \
	done
	@echo "Integration cluster ready"
	@echo "  ClickHouse nodes: localhost:19000, localhost:19001, localhost:19002"
	@echo "  Keeper:           localhost:19181, localhost:19182, localhost:19183"
	@echo "  OTEL Collector:   localhost:14317"

# Run integration tests (requires integration-up)
integration-test:
	go test -v -race -tags integration -timeout 300s ./...

# Stop the integration cluster
integration-down:
	docker compose -f testing/docker-compose.integration.yml down -v

# All-in-one: start cluster, run tests, stop cluster
integration: integration-up integration-test integration-down ## Start cluster, run integration tests, tear down (needs Docker)

# Run degradation simulation tests (docker pause/stop)
integration-degradation: integration-up
	go test -v -race -tags integration -run TestDegradation -timeout 300s ./...
	$(MAKE) integration-down

# ── Kubernetes ──────────────────────────────────────────────────────

# Validate k8s manifests: render kustomize + schema check via kubeconform
k8s-validate:
	@echo "==> Rendering kustomize..."
	@kubectl kustomize deploy/kubernetes/ > /dev/null
	@echo "  [ok] kustomize renders cleanly"
	@echo "==> Validating schemas..."
	@kubectl kustomize deploy/kubernetes/ | kubeconform -summary -strict
	@echo "  [ok] all resources valid"

# Render the Open Graph / Twitter card used by click-dog.com.
docs-social-card:
	@command -v $(RSVG_CONVERT) >/dev/null 2>&1 || { echo "FAIL: $(RSVG_CONVERT) not found; install librsvg or set RSVG_CONVERT=/path/to/rsvg-convert"; exit 1; }
	@$(RSVG_CONVERT) -w 1200 -h 630 docs/assets/social-card.svg -o docs/assets/social-card.png

# ── Release ─────────────────────────────────────────────────────────

# Run before tagging a release. Fails fast on any problem.
preflight: ## Pre-release gate (fmt, test, lint, govulncheck, build, config)
	@echo "==> Preflight checks for release"
	@echo ""
	@# Clean working tree
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "FAIL: working tree is dirty"; git status --short; exit 1; \
	fi
	@echo "  [ok] working tree clean"
	@# On master branch
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "master" ] && [ "$$BRANCH" != "main" ]; then \
		echo "FAIL: not on master/main (on $$BRANCH)"; exit 1; \
	fi
	@echo "  [ok] on $$(git rev-parse --abbrev-ref HEAD)"
	@# Code is gofmt-clean
	@echo "  [..] checking gofmt..."
	@$(MAKE) --no-print-directory fmt-check >/dev/null 2>&1 && echo "  [ok] gofmt clean" || { echo "FAIL: gofmt drift — run 'make fmt-check' for the file list, then 'make fmt'"; exit 1; }
	@# Tests pass
	@echo "  [..] running tests..."
	@go test -race ./... >/dev/null 2>&1 && echo "  [ok] tests pass" || { echo "FAIL: tests failed"; exit 1; }
	@# Lint passes
	@echo "  [..] running lint..."
	@golangci-lint run >/dev/null 2>&1 && echo "  [ok] lint clean" || { echo "FAIL: lint failed"; exit 1; }
	@# Vulnerability scan passes (release gate — see CI govulncheck job)
	@echo "  [..] running govulncheck..."
	@command -v govulncheck >/dev/null 2>&1 || { echo "FAIL: govulncheck not installed (run: go install golang.org/x/vuln/cmd/govulncheck@v1.3.0)"; exit 1; }
	@if out="$$(govulncheck ./... 2>&1)"; then \
		echo "  [ok] govulncheck clean"; \
	else \
		echo "FAIL: govulncheck reported vulnerabilities:"; \
		echo "$$out"; \
		exit 1; \
	fi
	@# Build succeeds
	@echo "  [..] building..."
	@go build -ldflags="-s -w -X main.version=preflight" -o /dev/null && echo "  [ok] build succeeds" || { echo "FAIL: build failed"; exit 1; }
	@# Config validates
	@echo "  [..] validating example config..."
	@go run -ldflags="-s -w" . -validate -config config.yaml.example >/dev/null 2>&1 && echo "  [ok] config.yaml.example validates" || echo "  [skip] config.yaml.example not validatable (expected if it has placeholders)"
	@# K8s manifests validate
	@echo "  [..] validating k8s manifests..."
	@kubectl kustomize deploy/kubernetes/ 2>/dev/null | kubeconform -summary -strict >/dev/null 2>&1 && echo "  [ok] k8s manifests valid" || echo "  [skip] k8s validation (kubeconform or kubectl not available)"
	@echo ""
	@echo "==> All preflight checks passed. Ready to tag."

# Tag a release. Auto-computes vYY.MM.idx, or override with v=YY.MM.idx.
# Optional qualifier: q=test|alpha|beta|ga (ga drops the suffix).
# Qualifier validation happens at parse time (see ifneq block above).
tag: ## Compute + create the next release tag (q=test|alpha|beta|ga)
	@TAG=""; \
	if [ -n "$(v)" ]; then \
		TAG="v$(v)$(QUAL_SUFFIX)"; \
	else \
		TAG="$(NEXT_TAG)"; \
	fi; \
	$(MAKE) preflight && \
	echo "" && \
	echo "==> Tagging $$TAG" && \
	git tag -a "$$TAG" -m "Release $$TAG" && \
	echo "" && \
	echo "Tag $$TAG created. To publish:" && \
	echo "  git push origin $$TAG" && \
	echo "" && \
	echo "On INTERNAL this runs release.yml as a gate only:" && \
	echo "  - Full test/lint/build/govulncheck/integration gate via ci.yml" && \
	echo "" && \
	echo "The artifact build (GoReleaser binaries + signed checksums + GitHub" && \
	echo "release + docker on ghcr.io/coltconsulting/click-dog) runs on the PUBLIC" && \
	echo "repo when you mirror the tag: 'make update-public PUSH=1'." && \
	echo "  (prerelease tags do NOT update :latest)"

# Shortcut: preflight + tag + push in one command.
# `&&` between the sub-make and push ensures a failed tag aborts the push.
release: ## preflight + tag + push in one step
	@TAG=""; \
	if [ -n "$(v)" ]; then \
		TAG="v$(v)$(QUAL_SUFFIX)"; \
	else \
		TAG="$(NEXT_TAG)"; \
	fi; \
	$(MAKE) tag v=$(v) q=$(q) && \
	git push origin "$$TAG"

# ── Public mirror ───────────────────────────────────────────────────

# Throw a GA release over the fence to the PUBLIC click-dog repo as a single
# squashed commit + tag. The tree comes from `git archive <tag>`, so no internal
# history, and .gitattributes export-ignore drops internal-only paths
# (docs/development, openspec, this tooling). Wraps scripts/publish-to-public.sh;
# see docs/development/specs/open-source-clean-slate-migration.md.
#
# DRY RUN by default — previews the squashed diff and stops. To actually commit
# and push, pass PUSH=1. Overrides:
#   TAG=vYY.MM.idx   release to publish (default: latest GA tag)
#   PUBLIC_DIR=path  clean checkout of the public repo (default: ../click-dog-public)
#
#   make update-public                 # preview latest GA release
#   make update-public TAG=v26.07.1    # preview a specific release
#   make update-public PUSH=1          # actually publish
update-public: ## Publish a GA release to the public repo (squash; DRY RUN unless PUSH=1)
	@test -f scripts/publish-to-public.sh || { \
		echo "update-public is internal-only — the publish tooling is export-ignored and not shipped to the public repo."; \
		exit 2; }
	@scripts/publish-to-public.sh $(TAG) \
		$(if $(PUBLIC_DIR),--public-dir $(PUBLIC_DIR),) \
		$(if $(filter 1 true yes,$(PUSH)),--push,)

# ── Docker ──────────────────────────────────────────────────────────

docker: build ## Build the Docker image for the current platform
	docker build --build-arg TARGETPLATFORM=. -t $(DOCKER_REPO):$(DOCKER_TAG) .

# Multi-platform images cannot be loaded into the classic Docker daemon.
# By default this writes an OCI archive; use `make docker-multiarch-push`
# to push the manifest list to $(DOCKER_REPO):$(DOCKER_TAG).
docker-multiarch:
	$(MAKE) build-all
	set -e; \
	trap '$(RM) -r .docker-buildx' EXIT; \
	$(RM) -r .docker-buildx; \
	mkdir -p .docker-buildx/linux/amd64 .docker-buildx/linux/arm64; \
	cp Dockerfile .docker-buildx/Dockerfile; \
	cp click-dog-linux-amd64 .docker-buildx/linux/amd64/click-dog; \
	cp click-dog-linux-arm64 .docker-buildx/linux/arm64/click-dog; \
	docker buildx build --platform linux/amd64,linux/arm64 --output "$(DOCKER_MULTIARCH_OUTPUT)" -t $(DOCKER_REPO):$(DOCKER_TAG) .docker-buildx

docker-multiarch-push: DOCKER_MULTIARCH_OUTPUT=type=registry
docker-multiarch-push: docker-multiarch

docker-push:
	docker push $(DOCKER_REPO):$(DOCKER_TAG)

# ── Dev ─────────────────────────────────────────────────────────────

run: ## Run locally against config.yaml
	go run . -config config.yaml

deps: ## Download + tidy Go module dependencies
	go mod download
	go mod tidy

# Both fmt and fmt-check operate on the filesystem (gofmt -w/-l .) rather than
# packages (go fmt ./...), so build-tagged files like integration_helpers_test.go
# are covered identically by both. If they diverged a contributor could hit
# the CI gate, run `make fmt`, see no change, and be stuck.
#
# Future: enabling the gofmt linter in a .golangci.yml would fold this gate
# into the existing golangci-lint job and remove the need for a separate target.
fmt: ## Format all Go files (gofmt -w)
	gofmt -w .

# Fail (don't fix) if any Go file is not gofmt-clean.
# Used by CI and `make preflight` so formatting drift can never reach master.
fmt-check: ## Fail if any Go file is not gofmt-clean (CI gate)
	@diff=$$(gofmt -l .); \
	if [ -n "$$diff" ]; then \
		echo "FAIL: the following Go files are not gofmt-clean:"; \
		echo "$$diff" | sed 's/^/  /'; \
		echo ""; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi

lint: ## Run golangci-lint
	golangci-lint run

# Run the same vulnerability scan that CI and `make preflight` enforce.
vulncheck: ## Run govulncheck (release gate)
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not installed; run: go install golang.org/x/vuln/cmd/govulncheck@v1.3.0"; exit 1; }
	govulncheck ./...

clean: ## Remove built binaries + coverage artifacts
	rm -f click-dog click-dog-linux-* coverage.out coverage.html

dev-setup:
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $$(go env GOPATH)/bin
