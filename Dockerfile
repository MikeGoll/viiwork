# viiwork experimental image built from the milpster gfx906 llama.cpp fork.
#
# This is the cutover for the experimental track (viiwork:gfx906). It
# replaces the old local, non-versioned fork tree at $GFX906_FORK
# (~/gfx906-work/llama.cpp-gfx906) with a build from a GitHub fork
# (milpster/gfx906-llama-cpp) pinned to a specific tag. That gives the
# track version control, reproducibility, and an upstream maintenance
# path — rolling the fork is just bumping MILPSTER_FORK_REF and rebuilding.
#
# Build via:
#
#   make docker-gfx906            # (or: make docker-experimental)
#
#   docker buildkit build -t viiwork:gfx906 \
#       -f Dockerfile.gfx906-milpster \
#       --build-arg MILPSTER_FORK_REF=b10803 .
#
# The old Dockerfile.gfx906 (local-fork build context) is preserved as the
# rollback path, matching the repo convention of keeping a rollback image.

# -----------------------------------------------------------------------------
# Stage 1: Build milpster gfx906-llama-cpp with ROCm/gfx906
# -----------------------------------------------------------------------------
FROM rocm/dev-ubuntu-24.04:6.2.4-complete AS llama-build
RUN apt-get update && apt-get install -y --no-install-recommends cmake git \
    && rm -rf /var/lib/apt/lists/*

# Pinned to a GitHub tag (not a moving branch) so rebuilds are deterministic.
# milpster/gfx906-llama-cpp is a ggml-org/llama.cpp fork specialized for
# AMD gfx906 (Radeon VII / Instinct MI50–MI60). It inherits upstream arch
# coverage — this pin (b10803) carries muse-glimmer, DeltaNet/qwen3_moe,
# and MTP speculative-decoding support.
ARG MILPSTER_FORK_URL=https://github.com/milpster/gfx906-llama-cpp
ARG MILPSTER_FORK_REF=b10803
RUN git clone --depth 1 --branch ${MILPSTER_FORK_REF} ${MILPSTER_FORK_URL} /llama.cpp
WORKDIR /llama.cpp

# FP8 handling already ships in the fork (the gfx906 FP8 header issue is
# resolved upstream here — __hip_fp8_e4m3 is gated by HIP_VERSION and the
# usage sites are guarded by FP8_AVAILABLE), so no in-place sed patch is
# needed the way the stock upstream build requires. Keep a loud, conditional
# build-time assertion so a future ROCm/fork break fails the build visibly
# instead of at 04:42 in a container log.
RUN set -eux; \
    f=ggml/src/ggml-cuda/vendors/hip.h; \
    if [ ! -f "$f" ]; then \
        echo "FATAL: $f does not exist -- fork layout changed."; exit 1; \
    fi; \
    if grep -q 'HIP_VERSION >= 60200000' "$f" && \
       grep -q '__hip_fp8_e4m3 __nv_fp8_e4m3' "$f"; then \
        echo "OK: gfx906 FP8 handling present in fork."; \
    else \
        echo "############################################################"; \
        echo "FATAL: expected FP8 handling not found in $f."; \
        echo "This fork should ship the gfx906 FP8 fix. Current fp8 lines:"; \
        grep -n -i 'fp8\|HIP_VERSION' "$f" || true; \
        echo "Use a fork ref that includes the fix, or port the small guard."; \
        echo "############################################################"; \
        exit 1; \
    fi

# BUILD_SHARED_LIBS=OFF -> self-contained static binaries, no .so shuffling
# in the runtime stage (the fragile find -name '*.so*' step is dropped).
# GPU_TARGETS + AMDGPU_TARGETS -> newer CMake prefers the former, this
# fork/HIP still reads the latter. CMAKE_POSITION_INDEPENDENT_CODE=ON is
# REQUIRED for the static build on Ubuntu 24.04 (hipcc emits non-PIC objects
# into libggml-hip.a, which breaks the default PIE linker step).
RUN cmake -B build \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
        -DLLAMA_CURL=OFF \
        -DGGML_HIP=ON \
        -DGPU_TARGETS=gfx906 \
        -DAMDGPU_TARGETS=gfx906 \
        -DGGML_HIP_ROCWMMA_FATTN=OFF \
    && cmake --build build --config Release -j$(nproc) \
        --target llama-server llama-cli

# Smoke test: both binaries run and can resolve the HIP runtime. llama-cli
# is included alongside llama-server for rocprof-based kernel profiling.
RUN ldd /llama.cpp/build/bin/llama-server && /llama.cpp/build/bin/llama-server --help > /dev/null
RUN ldd /llama.cpp/build/bin/llama-cli    && /llama.cpp/build/bin/llama-cli    --help > /dev/null

# Assertion: the architecture is genuinely registered in this build. If this
# fails, MILPSTER_FORK_REF predates the arch merge — do not ship the image.
RUN if ! grep -qa 'muse-glimmer' /llama.cpp/build/bin/llama-server; then \
        echo "FATAL: 'muse-glimmer' arch not found in llama-server."; \
        echo "Use a MILPSTER_FORK_REF that carries the hybrid/MTP/muse arches."; \
        exit 1; \
    fi; \
    echo "OK: muse-glimmer architecture present."

# -----------------------------------------------------------------------------
# Stage 2: Build viiwork (unchanged from the original Dockerfile)
# -----------------------------------------------------------------------------
FROM golang:1.27.0 AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /viiwork ./cmd/viiwork

# -----------------------------------------------------------------------------
# Stage 3: Runtime (unchanged from the original Dockerfile)
# -----------------------------------------------------------------------------
FROM rocm/dev-ubuntu-24.04:6.2.4-complete

RUN apt-get update && apt-get install -y --no-install-recommends ipmitool \
    && rm -rf /var/lib/apt/lists/*

COPY --from=llama-build /llama.cpp/build/bin/llama-server /usr/local/bin/
COPY --from=llama-build /llama.cpp/build/bin/llama-cli    /usr/local/bin/
COPY --from=go-build    /viiwork                          /usr/local/bin/

# Required: ROCm may not natively recognize gfx906 in all versions;
# this override forces gfx900-series compatibility.
ENV HSA_OVERRIDE_GFX_VERSION=9.0.6

# Surface llama-server's own stdout/stderr.
ENV GGML_LOG_LEVEL=info

EXPOSE 8080
ENTRYPOINT ["viiwork"]
CMD ["--config", "/etc/viiwork/viiwork.yaml"]
