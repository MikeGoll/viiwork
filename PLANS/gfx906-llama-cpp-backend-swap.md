# Plan: Swap viiwork's llama.cpp backend to `milpster/gfx906-llama-cpp`

> **Status: analysis / plan only.** No source or build changes have been made.
> Items marked **VERIFY FIRST** depend on information I could not obtain here
> (GitHub network access is blocked in plan mode). Do not skip them — the whole
> feasibility of the swap hinges on the answers.

## Objective

Replace the `llama.cpp` backend viiwork spawns at runtime with a build from
`milpster/gfx906-llama-cpp` (a GitHub fork specialized for AMD gfx906 — Radeon
VII / Instinct MI50–MI60), in place of the current **locally-built, non-versioned
fork tree** at `$GFX906_FORK` (`~/gfx906-work/llama.cpp-gfx906`) used by the
`viiwork:gfx906` experimental track. Goal: make that track buildable from a
referenced Git commit/tag (version control, reproducibility, upstream maintenance)
without touching viiwork's Go server logic.

## Analysis

### Why this is mostly a build/Docker change, not a Go change

viiwork is **backend-agnostic**. The Go server never calls into llama.cpp — it
spawns one process per backend and talks to it **only over HTTP** (`internal/process/backend.go`):

- **Spawn:** `exec.Command(b.Binary, b.buildArgs()...)`. Binary path is a config
  value, default `"llama-server"` (`internal/config/defaults.go` → `BackendConfig.Binary`); env set per-backend via `ROCR_VISIBLE_DEVICES`.
- **Liveness:** `GET /health`
- **Monitoring:** `GET /slots` (parses `n_ctx`, `is_processing`, `next_token.{n_decoded,n_remain}`)
- **Inference:** `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`

So any replacement `llama-server` that (a) runs on gfx906 via ROCm/HIP and
(b) speaks the same CLI flags + HTTP endpoints drops in with **zero Go changes** —
only rebuild the binary and update the Dockerfile.

### CLI flags viiwork relies on (must all exist in the new fork)

`--model --port --ctx-size --n-gpu-layers --parallel --slots --log-disable`
`--split-mode (layer|row|tensor) --tensor-split --main-gpu --threads`
plus user `extra_args`. Repo default is `["--reasoning-format","deepseek"]`
(`defaults.go`); deployed configs add `--jinja`, `--chat-template-kwargs`,
`--cache-type-k/v`, `--split-mode layer`, `-fa`, `-ub`, etc.

### The two existing tracks

| Track | Image | Dockerfile | llama.cpp source |
|---|---|---|---|
| Stable | `viiwork:latest` | `Dockerfile` | upstream `ggml-org/llama.cpp` @ pinned tag (e.g. `b10453`) |
| Experimental | `viiwork:gfx906` | `Dockerfile.gfx906` | **local fork tree** at `$GFX906_FORK` (unversioned, not in repo) |

`make docker-gfx906` requires `$GFX906_FORK/.git` and injects it via DockerKit
`--build-context fork=…` — the fragility a GitHub fork removes.

### Feasibility (confirmed with the requester)

`milpster/gfx906-llama-cpp` is a **different fork than** the existing
`llama.cpp-gfx906` strip-down, but three feasibility blockers that could have
blocked the swap are now **confirmed resolved**:

1. **Base = upstream `ggml-org/llama.cpp`** ✅ — so it inherits upstream's
   arch coverage. It *may* carry Qwen3.5/3.6/3.8 hybrid DeltaNet, MTP, and
   `muse-glimmer` (verify the exact base commit in Step 1); either way it is at
   least as broad as the current arch-pruned `viiwork:gfx906` fork, never narrower.
2. **All required flags/routes present** ✅ — `--slots`, `--split-mode`,
   `--tensor-split`, `--parallel`, `--reasoning-format`, `--main-gpu`,
   `--log-disable`, plus `/health` + `/slots`. This removes the highest-risk
   concern (a stripped fork dropping `--slots` / the JSON shape viiwork parses).
3. **FP8 header compile handled** ✅ — the fork already ships the gfx906 FP8 fix,
   so no `ggml/src/ggml-cuda/vendors/hip.h` patch is needed (still assert it at
   build time so a future break is loud, per the stable `Dockerfile` pattern).

**Remaining Step-1 checks (low risk, still verify):** the exact upstream base
commit / whether hybrid+MTP+muse arches are present in *this* fork, the tag/branch
layout to pick a pin, and the ROCm version it targets (match
`rocm/dev-ubuntu-24.04:6.2.4-complete`).

(I could not fetch the repo — `curl`/`git` blocked in plan mode — so these are
the requester's confirmed answers, not machine-verified.)

## Steps

### Step 0 — Decide scope (file changes assume (a))
- **(a) Replace the experimental track** — `viiwork:gfx906` builds from
  `milpster/gfx906-llama-cpp @ <commit>`; local `$GFX906_FORK` dependency removed.
  Minimal, low-risk. ✅ **recommended.**
- **(b) Add a third track** `viiwork:gfx906-milpster`. Keeps everything; more surface.
- **(c) Replace BOTH tracks** — do not do this; stable intentionally runs stock upstream.

### Step 1 — Confirm build details & pick a pin (network; low risk now)
Confirmed by requester (machine-verify during implementation):
- Base = upstream `ggml-org/llama.cpp`; required flags/`/slots`/`/health` present;
  gfx906 FP8 header issue already handled.
Remaining checks:
- Read README: exact upstream base commit, ROCm target, build instructions. Pick a
  specific commit/tag to pin for reproducibility.
- Note whether hybrid DeltaNet / MTP / `muse-glimmer` are present in *this* fork
  (`grep -ra muse-glimmer src/`), and the ROCm version (aim for
  `rocm/dev-ubuntu-24.04:6.2.4-complete`).
- Quick build sanity: `cmake -B build -DGGML_HIP=ON -DAMDGPU_TARGETS=gfx906` on the
  pinned ref, build `llama-server`, smoke `--help`.
- Deliverable = the pinned ref + ROCm pin. If the fork fails to build on the target
  ROCm, match ROCm or port the small fix (not expected — FP8 is already handled).

### Step 2 — New Dockerfile `Dockerfile.gfx906-milpster` (mirrors `Dockerfile.gfx906`)
```dockerfile
FROM rocm/dev-ubuntu-24.04:6.2.4-complete AS llama-build
ARG MILPSTER_FORK_URL=https://github.com/milpster/gfx906-llama-cpp
ARG MILPSTER_FORK_REF=<pinned-commit>
RUN git clone --depth 1 --branch ${MILPSTER_FORK_REF} ${MILPSTER_FORK_URL} /llama.cpp
WORKDIR /llama.cpp
# FP8 fix already ships in the fork (confirmed). Keep a loud, conditional
# build-time assertion so a future ROCm break fails the build visibly:
#   f=ggml/src/ggml-cuda/vendors/hip.h; grep -q '99999999' "$f" && echo OK || { echo FATAL: fp8 guard missing; exit 1; }
RUN cmake -B build -DGGML_HIP=ON -DGPU_TARGETS=gfx906 -DAMDGPU_TARGETS=gfx906 \
        -DGGML_HIP_ROCWMMA_FATTN=OFF -DBUILD_SHARED_LIBS=OFF \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
    && cmake --build build --target llama-server llama-cli -j$(nproc)
# Smoke test + arch assertion as in Dockerfile.gfx906.
```
Prefer `BUILD_SHARED_LIBS=OFF` (static, like stable `Dockerfile`) to drop the
fragile `.so`-copy step. Keep `HSA_OVERRIDE_GFX_VERSION=9.0.6` and the
`llama-server`/`llama-cli` copies in the runtime stage.

### Step 3 — Makefile: replace `$GFX906_FORK` guard with fork clone
```make
MILPSTER_FORK_URL ?= https://github.com/milpster/gfx906-llama-cpp
MILPSTER_FORK_REF ?= <pinned-commit>
docker-gfx906 docker-experimental:
	DOCKER_BUILDKIT=1 docker build -t viiwork:gfx906 \
	    -f Dockerfile.gfx906-milpster \
	    --build-arg MILPSTER_FORK_URL=$(MILPSTER_FORK_URL) \
	    --build-arg MILPSTER_FORK_REF=$(MILPSTER_FORK_REF) \
	    --build-arg VERSION=$(VERSION)-gfx906 .
```
Drop the `@test -d "$(GFX906_FORK)/.git"` guard; Go/build logic otherwise untouched.

### Step 4 — Switch tooling & setup prompt
- `scripts/switch-node-build.sh`: no change if image stays `viiwork:gfx906`.
- `scripts/setup-node.sh`: keep the 1-stable / 2-gfx906 prompt; update the two
  comment blocks describing option 2 from "local fork tree" → "GitHub fork @ commit".

### Step 5 — Docs
- **BUILDS.md:** swap experimental-track "llama.cpp source" row; update build +
  image-distribution/rollback (the `docker save | docker load` workaround is no
  longer needed for nodes that can clone).
- **README.md → Builds section:** same source-line change + "no local fork needed".
- Rolling the fork = bump `MILPSTER_FORK_REF` in the Makefile and rebuild.

### Step 6 — Re-verify & re-bench (regression gate; use `bench-harness/` +
`milestone/gfx906-fork-4h-soak-2026-04-09`)
1. **Functional:** run a `configs/` example (e.g. `viiwork.gptoss-120b-5pairs.yaml`
   hybrid MoE, `viiwork.gemma4-31b-ts2.yaml` tensor-split); confirm every model loads.
2. **Endpoint parity:** `/health`, `/slots` (JSON shape), real `/v1/chat/completions`.
3. **Tensor-split:** confirm `--split-mode layer` works; `row` limitation unchanged.
4. **A/B soak:** 4h @ conc-10 vs stable → confirm ≥ +3% sustained tok/s and 0
   failures with bounded RSS/VRAM. Newer upstream = bonus hybrid-model support.
5. **Size audit:** diff `llama-model.cpp` line counts vs current fork if slimming is the point.

## Files to Modify

- **`Dockerfile.gfx906-milpster`** *(new)* — builds milpster fork @ pinned ref,
  replicates the gfx906 FP8 fix. Separate file preserves the current fork as a
  rollback (matches repo convention of keeping `Dockerfile` as stable rollback).
  Alternatively edit `Dockerfile.gfx906` in place for a single file.
- **`Makefile`** — replace `$GFX906_FORK`-guard build with fork-clone build; add
  `MILPSTER_FORK_URL` / `MILPSTER_FORK_REF` args. Go/build logic untouched.

## Files to Update (docs/scripts, no logic change)

- `BUILDS.md`, `README.md` — experimental-source row + build/distribution/rollback.
- `scripts/switch-node-build.sh` — only if the image tag changes.
- `scripts/setup-node.sh` — comment text for option 2 description.

## Risks & Considerations

- **Flag/route regressions:** downgraded — requester confirmed all required flags
  and `/health`+`/slots` are present. Still verify the exact `/slots` JSON shape
  matches what `internal/process/backend.go` parses, and that the pinned ref hasn't
  shifted it.
- **Arch coverage:** downgraded — fork is on upstream `ggml-org/llama.cpp`, so it is
  at least as broad as the current arch-pruned fork. Just confirm the exact base
  commit carries the hybrid/MTP/muse arches the reference fleet needs (they may be a
  bonus vs the current `viiwork:gfx906`).
- **FP8 compile / ROCm pin:** downgraded — fork already handles FP8; assert it loudly
  at build time rather than patching. Match `rocm/dev-ubuntu-24.04:6.2.4-complete`
  ("last reliable gfx906") unless the fork targets something newer and proven.
- **Build-time network / reproducibility:** pin a commit (not a moving branch) so
  rebuilds are deterministic; ensure build hosts have outbound git access.
- **Loss of local-fork baseline:** preserve `llama.cpp-gfx906` as a rollback
  Dockerfile if you drop it from the Makefile.
- **The confirmed answers are the requester's, not machine-verified** — re-confirm the
  base commit, `/slots` JSON shape, and ROCm pin during implementation.
- **Loss of local-fork baseline:** preserve `llama.cpp-gfx906` as a rollback
  Dockerfile if you drop it from the Makefile.
- **Build-time network:** Docker now clones from GitHub at build time (likely fine —
  the stable track already builds from upstream).
- **This plan could not verify the milpster repo** (no network in plan mode).
  Step 1 must run before any code change.
