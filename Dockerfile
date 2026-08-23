# =============================================================================
# viiwork + llama.cpp (ROCm / gfx906) with Muse Glimmer support
#
# Changes vs. the previous image:
#   1. LLAMA_CPP_VERSION bumped b9222 -> b10361.
#      Muse Glimmer support merged 2026-08-10 as commit 62bf73d (PR #26841)
#      and first shipped in release b10353. Anything <= b10344 rejects the
#      GGUF with "unknown model architecture 'muse-glimmer'".
#   2. The gfx906 FP8 header patch is now conditional and LOUD. The old
#      unconditional `sed` silently no-ops if upstream moves that line,
#      which would resurface the FP8 compile error with no explanation.
#   3. Static build (BUILD_SHARED_LIBS=OFF). Removes the fragile
#      `find -name '*.so*' -exec cp` step in the runtime stage entirely.
#   4. Build-time assertion that the muse-glimmer arch string is actually
#      compiled into llama-server, so a bad pin fails the build instead of
#      failing at 04:42 in a container log.
#   5. llama-mtmd-cli added for testing the vision projector (--mmproj).
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Build llama.cpp with ROCm/gfx906
# Pinned to ROCm 6.2.4 -- last version with reliable gfx906 support.
# -----------------------------------------------------------------------------
FROM rocm/dev-ubuntu-24.04:6.2.4-complete AS llama-build

RUN apt-get update && apt-get install -y --no-install-recommends \
        cmake git build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# b10361: contains 62bf73d (muse-glimmer arch, vision projector, ATEM
# tool-call parser, DFlash speculative decoding). b10353 is the floor.
#ARG LLAMA_CPP_VERSION=b10361
ARG LLAMA_CPP_VERSION=b10453
RUN git clone --branch ${LLAMA_CPP_VERSION} --depth 1 \
    https://github.com/ggml-org/llama.cpp /llama.cpp
WORKDIR /llama.cpp

# gfx906 (Radeon VII) has no FP8 hardware; the HIP 6.2+ header defines
# __hip_fp8_e4m3 only for newer archs, causing a compile error.
# Fail visibly rather than silently if upstream has restructured this.
RUN set -eux; \
    f=ggml/src/ggml-cuda/vendors/hip.h; \
    if [ ! -f "$f" ]; then \
        echo "FATAL: $f does not exist -- upstream layout changed."; exit 1; \
    fi; \
    if grep -q 'HIP_VERSION >= 60200000' "$f"; then \
        sed -i 's/#if HIP_VERSION >= 60200000/#if HIP_VERSION >= 99999999/' "$f"; \
        echo "OK: FP8 guard disabled for gfx906."; \
    else \
        echo "############################################################"; \
        echo "WARNING: FP8 guard pattern not found in $f."; \
        echo "Upstream changed this file. Current fp8-related lines:"; \
        grep -n -i 'fp8\|HIP_VERSION' "$f" || true; \
        echo "If the build fails below on __hip_fp8_e4m3, patch manually."; \
        echo "############################################################"; \
    fi

# BUILD_SHARED_LIBS=OFF  -> self-contained binaries, no .so shuffling.
# LLAMA_CURL=OFF         -> models are local files; drops the libcurl dep.
# GPU_TARGETS + AMDGPU_TARGETS -> newer CMake prefers the former, older
#                                 llama.cpp/HIP still reads the latter.
#
# CMAKE_POSITION_INDEPENDENT_CODE=ON is REQUIRED for the static build on
# Ubuntu 24.04. GCC there defaults to -pie for executables, but hipcc emits
# non-PIC objects into libggml-hip.a when BUILD_SHARED_LIBS=OFF, producing:
#   relocation R_X86_64_32 against `.rodata.str1.1' can not be used when
#   making a PIE object
# This forces -fPIC onto the static library targets, HIP sources included.
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
        --target llama-server llama-perplexity llama-mtmd-cli

# Smoke test: binaries run and can resolve the HIP runtime.
RUN /llama.cpp/build/bin/llama-server      --help > /dev/null \
 && /llama.cpp/build/bin/llama-perplexity  --help > /dev/null \
 && /llama.cpp/build/bin/llama-mtmd-cli    --help > /dev/null

# Assertion: the architecture is genuinely registered in this build.
# If this fails, LLAMA_CPP_VERSION is too old -- do not ship the image.
RUN if ! grep -qa 'muse-glimmer' /llama.cpp/build/bin/llama-server; then \
        echo "FATAL: 'muse-glimmer' arch not found in llama-server."; \
        echo "LLAMA_CPP_VERSION=${LLAMA_CPP_VERSION} predates PR #26841."; \
        echo "Use b10353 or newer."; \
        exit 1; \
    fi; \
    echo "OK: muse-glimmer architecture present."

# -----------------------------------------------------------------------------
# Stage 2: Build viiwork
# -----------------------------------------------------------------------------
FROM golang:1.26.6 AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /viiwork ./cmd/viiwork

# -----------------------------------------------------------------------------
# Stage 3: Runtime
# -----------------------------------------------------------------------------
FROM rocm/dev-ubuntu-24.04:6.2.4-complete

RUN apt-get update && apt-get install -y --no-install-recommends \
        ipmitool \
    && rm -rf /var/lib/apt/lists/*

COPY --from=llama-build /llama.cpp/build/bin/llama-server     /usr/local/bin/
COPY --from=llama-build /llama.cpp/build/bin/llama-perplexity /usr/local/bin/
COPY --from=llama-build /llama.cpp/build/bin/llama-mtmd-cli   /usr/local/bin/
COPY --from=go-build    /viiwork                              /usr/local/bin/

# Required: ROCm may not natively recognize gfx906 in all versions;
# this override forces gfx900-series compatibility.
ENV HSA_OVERRIDE_GFX_VERSION=9.0.6

# Surface llama-server's own stdout/stderr. The previous failure was
# invisible because nothing from llama.cpp reached the container log.
ENV GGML_LOG_LEVEL=info

EXPOSE 8080
ENTRYPOINT ["viiwork"]
CMD ["--config", "/etc/viiwork/viiwork.yaml"]
