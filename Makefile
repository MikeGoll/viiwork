.PHONY: build mcp test clean docker docker-stable docker-gfx906 docker-experimental up down

# Experimental track source: the milpster gfx906 llama.cpp fork on GitHub.
# Pinned to a tag (not a moving branch) for reproducible rebuilds. Bump
# MILPSTER_FORK_REF and rebuild to roll the fork upstream; no local clone
# required — the Dockerfile clones it at build time.
#   b10803 = ggml-org/llama.cpp fork specialized for AMD gfx906
#            (Radeon VII / Instinct MI50–MI60); carries muse-glimmer,
#            DeltaNet/qwen3_moe, and MTP arches.
MILPSTER_FORK_URL ?= https://github.com/milpster/gfx906-llama-cpp
MILPSTER_FORK_REF ?= b10803

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork ./cmd/viiwork

mcp:
	go build -o bin/viiwork-mcp ./cmd/viiwork-mcp

test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... -v; \
	else \
		echo "go not found on host, running tests in container..."; \
		docker run --rm -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false golang:1.26.6 go test ./... -v; \
	fi

clean:
	rm -rf bin/

# === Docker builds ===
# viiwork ships in two parallel images that share the Go server but
# differ in the llama.cpp binary they spawn. See BUILDS.md for the
# full comparison and rollout guidance.
#
#   docker / docker-stable           -> viiwork:latest  (upstream llama.cpp)
#   docker-gfx906 / docker-experimental -> viiwork:gfx906 (milpster gfx906 fork)
#
# The two pairs are aliases so the Makefile reads symmetrically with
# the language used in BUILDS.md and scripts/setup-node.sh, while
# keeping the original target names working for older docs and habits.

# Stable foundation: standard upstream llama.cpp from the default Dockerfile.
docker docker-stable:
	docker build -t viiwork .

# Experimental track: gfx906-specialized fork build from GitHub.
# Clones ${MILPSTER_FORK_URL} @ ${MILPSTER_FORK_REF} inside the Dockerfile
# (Dockerfile.gfx906-milpster), so no local fork tree or --build-context is
# needed anymore. Go/build logic is otherwise untouched by the swap.
docker-gfx906 docker-experimental:
	DOCKER_BUILDKIT=1 docker build \
	    -t viiwork:gfx906 \
	    -f Dockerfile.gfx906-milpster \
	    --build-arg MILPSTER_FORK_URL=$(MILPSTER_FORK_URL) \
	    --build-arg MILPSTER_FORK_REF=$(MILPSTER_FORK_REF) \
	    --build-arg VERSION=$(VERSION)-gfx906 \
	    .

up:
	docker compose up -d

down:
	docker compose down
