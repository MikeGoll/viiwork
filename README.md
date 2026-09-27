# viiwork

**LLM inference for a fleet of machines.** One viiwork node per machine runs
every model configured on it — each model's backends pinned to their own GPUs —
behind a single OpenAI-compatible API. Nodes find each other and form a mesh on
their own: any node is an entry point, and a request goes to a free slot wherever
one exists in the fleet.

Three inference engines (`llamacpp`, `vllm`, `freetoken`), on ROCm or CUDA, in
one binary whose only dependencies are `yaml.v3`, `memberlist` and `mdns`.

![viiwork mesh dashboard](docs/img/viiwork-v220.webp)

> **Upgrading from viiwork 1.x?** v2 changed the mesh protocol, so **a v1 node
> and a v2 node never see each other** — convert a mesh together, or run the two
> as separate meshes until the last machine is across. The config is a different
> file, and a v1 one is refused at startup rather than guessed at.
> **[docs/migrating-to-v2.md](docs/migrating-to-v2.md)** maps every key, works a
> real machine through the change and covers rollback. viiwork 1.x remains at tag
> [`v1.8.1`](https://github.com/janit/viiwork/releases/tag/v1.8.1).

## Background

I had 50 Radeon VII cards sitting in servers in my mother-in-law's garage (who
doesn't?) and wanted to do something useful with them. viiwork was born out of
that — a way to turn a pile of aging-but-capable GPUs into a practical LLM
inference cluster. The Radeon VII and the Instinct MI50/MI60 are all gfx906 cards
with 16 GB of HBM2 (32 GB for the MI60) and a 1 TB/s memory bus: legacy hardware
that punches well above its weight for inference, where memory bandwidth is the
bottleneck, and cheap secondhand.

That fleet is still the reference deployment and still where the measurements in
[docs/models.md](docs/models.md) come from, but viiwork is no longer tied to it.
Engines are named for the engine, never for a GPU vendor — vLLM has a ROCm build,
FreeToken runs on CUDA, and nothing in viiwork pairs the two. It is meant to be
useful at any scale: one old gaming GPU on a desktop, a few Radeon Pro VII cards
in a workstation, or racks of MI50s in your mother-in-law's garage.

## What you get

- **One process per machine, one API.** Every model on the box behind port 8086,
  OpenAI-compatible → [configuration](docs/configuration.md)
- **Three engines in one binary.** llama.cpp, vLLM and FreeToken side by side on
  one mesh — including sparse MoE models far larger than a single card, through
  FreeToken's expert offload →
  [large models on one card](docs/models.md#large-models-on-one-card-freetoken-and-moe-offload)
- **A mesh that assembles itself.** No peer lists: nodes discover each other over
  a tailnet or the LAN and route to whoever has a free slot →
  [the mesh](docs/mesh.md)
- **Mesh-wide aliases.** Point clients at `stable-coder` and switch what it means
  once, from any machine → [aliases](docs/mesh.md#aliases)
- **Live cluster dashboards.** Models, in-flight jobs, backends, prompts and
  power for the whole fleet, served identically by every node →
  [dashboards](docs/dashboards.md)
- **`viiwork top`.** The whole mesh live in a terminal, with a per-host
  drill-down → [operations](docs/operations.md#watching-the-mesh-viiwork-top)
- **Power, cost and energy accounting.** Per-host wattage, ENTSO-E spot cost, and
  a durable per-model kWh history → [power and energy](docs/power-and-energy.md)
- **Pipelines.** Chain several LLM steps into one virtual model name →
  [pipelines](docs/configuration.md#pipelines)
- **Chassis power control.** Switch fleet hosts on and off from the dashboard,
  through IPMI → [power control](docs/power-and-energy.md#power-control)
- **An MCP server.** Expose the cluster as tools to any MCP-compatible assistant
  → [MCP](docs/operations.md#mcp-server)
- **The fleet describes itself.** Point a coding client at any node and it
  discovers the models and the context window it can rely on, with no list to
  maintain and no numbers typed in → [autodiscovery](docs/autodiscovery.md)

## Quick Start

Download a release, check it, and run the setup wizard:

```bash
v=vX.Y.Z                     # from https://github.com/janit/viiwork/releases
os=linux_amd64               # or linux_arm64, darwin_arm64
base=https://github.com/janit/viiwork/releases/download/$v
curl -fLO "$base/viiwork_${v}_${os}.tar.gz" -fLO "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS   # macOS: shasum -a 256 --check --ignore-missing SHA256SUMS
tar xzf "viiwork_${v}_${os}.tar.gz" && cd "viiwork_${v}_${os}"

sudo ./viiwork init          # Linux; on a Mac: ./viiwork init (no sudo)
```

The wizard finds the GPUs and the GGUF models in a directory you name. It
proposes a layout and asks whether to start a new mesh or join one. Then it
shows every file it will write. On a yes, it writes them, starts the node (in
Docker on Linux, under a LaunchAgent on a Mac) and checks it. Nothing is
written before that yes. → [setup](docs/setup.md)

**The next machine** joins with one code. On any node, print it:

```bash
sudo sh -c 'set -a; . /etc/viiwork/mesh.env; viiwork join-code'   # Linux
viiwork join-code                                                  # Mac
```

Run `viiwork init` on the new machine and paste the code at the mesh
question. The code *is* the mesh secret, so handle it like one.

**Test it:**

```bash
curl http://localhost:8086/v1/models
curl http://localhost:8086/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"<a model name from /v1/models>","messages":[{"role":"user","content":"Hello"}]}'
```

The API and every dashboard are on port 8086, and membership gossip is on 7946
(tcp and udp), on every machine. `viiwork top` watches the whole mesh from a
terminal. `viiwork uninstall` removes exactly what the wizard installed, and
keeps your model files.

### By hand

The wizard starts a node on NVIDIA GPUs and on Apple Silicon. On a Radeon VII
(gfx906) or another AMD card it writes the config and stops, because those need
an image built from a checkout. Set up this way, or to lay a machine out
yourself:

```bash
# 1. Write the machine's config
cp viiwork.yaml.example viiwork.yaml
# Edit viiwork.yaml: one entry under models: per model, with its GPUs.

# 2. Choose the mesh mode: a shared secret, or mesh.open: true in viiwork.yaml
sudo install -d /etc/viiwork && sudo install -m 0640 /dev/null /etc/viiwork/mesh.env
echo "VIIWORK_MESH_SECRET=$(openssl rand -base64 32)" | sudo tee /etc/viiwork/mesh.env >/dev/null

# 3. Build and run
make docker
cp configs/docker-compose.v2.example.yaml docker-compose.yaml
docker compose up -d
```

Every machine runs the same image with its own `viiwork.yaml`.

## Configuration

A machine's models are entries under `models:` in its one config file. Each has
an explicit `name` (the model id clients send), its host GPU indices, and its
slots:

```yaml
models:
  - name: Qwen3.8-27B
    engine: llamacpp
    path: /models/Qwen3.8-27B-UD-Q4_K_XL.gguf
    gpus: [0, 1]
    gpus_per_backend: 2      # one backend across both cards
    context: 49152           # tokens PER SLOT
    parallel: 2
  - name: granite-4.2-8b
    engine: llamacpp
    path: /models/granite-4.2-8b-Q4_K_M.gguf
    gpus: [2, 3, 4, 5]       # gpus_per_backend defaults to 1: four replicas
    context: 16384
    parallel: 2
```

A GPU belongs to at most one model, and every model on every machine is visible
from any node. `SIGHUP` (`docker kill -s HUP viiwork`) applies an edited model
list: added models start, removed ones drain, a changed entry restarts that model
only.

The file is the only input — there are no command-line overrides — and the node
validates all of it at startup and on every reload, naming the offending field
when something is wrong. **`models[].context` is the context of one slot**, for
every engine.

Full reference, including tensor-split, pipelines, GPU power limits, environment
variables and host requirements:
**[docs/configuration.md](docs/configuration.md)**.

## The mesh

Nodes form one mesh without peer lists. Switch a machine on and its models are
usable from every node within seconds; switch it off and routing stops sending to
it first.

```yaml
mesh:
  network: tailnet     # tailnet (default) | lan
  seeds: []            # optional "ip:7946" of any member
```

**Two fixed ports on every machine:** 8086 tcp for the API and the dashboards,
7946 tcp+udp for gossip. On a tailnet a node advertises its Tailscale address and
finds the others through tailscaled; on a LAN it advertises its default-route
address and finds them with mDNS.

**Secured or open, never neither.** Set `VIIWORK_MESH_SECRET` on every machine
and gossip is encrypted, forwards are signed and alias writes need a signature;
leave it unset and set `mesh.open: true` and any node that reaches 7946 joins. A
node with neither refuses to start.

**Routing follows free slots.** A request goes to a local backend with a free
slot, else to the member with the most free slots, else it queues on the node
that received it for up to `routing.queue_timeout` (20 s). Capacity is polled
once a second; membership is gossiped.

Details, including secret rotation without downtime and how refusals are retried:
**[docs/mesh.md](docs/mesh.md)**.

## Dashboards

| Page | What it is |
|---|---|
| `/` | This node: models, backends, GPU and VRAM graphs, activity |
| `/mesh` | The whole cluster — models, aliases, in-flight jobs, backends, fleet power — served identically by every node |
| `/chat` | A lightweight chat UI (`?model=<id>&host=<node>`) |
| `/prompt` | One request's prompt and output, full page |

The browser never polls: `/mesh` opens a single SSE stream, and the node you
opened reaches the other members itself, so they need not be reachable from where
you are viewing. **[docs/dashboards.md](docs/dashboards.md)**.

## Engines

An engine is named in a model's `engine:` key, and the YAML block below it
belongs to that engine.

| Engine | What it drives | Shape | Readiness |
|---|---|---|---|
| `llamacpp` | `llama-server`, the reference implementation | runs on CPU or GPU | `/health`, slots from `/slots` |
| `vllm` | `vllm serve` | one backend tensor-parallel across its cards | `/health` 200, occupancy from `/metrics` |
| `freetoken` | `ft serve` | **one card per process**, MoE experts offloaded to host memory | the `/health` *body*, occupancy from `/v1/stats` |

All three can run on one node at once, each model's backends pinned to their own
cards — the reference fleet runs llama.cpp on five ROCm hosts, vLLM on one and
FreeToken on two, in one mesh behind one API.

Two things about FreeToken are worth knowing. Its **readiness** cannot be taken
from the status code: it answers `/health` with 200 in *every* lifecycle state,
including while loading, so the code alone would advertise a backend that 503s
every request. And it is the engine that **runs models bigger than the card** —
one GPU per process, with a sparse MoE's cold experts streaming from host memory
rather than spread across cards. See
[models.md](docs/models.md#large-models-on-one-card-freetoken-and-moe-offload).

**Adding one is one package plus one line in `internal/engine/all`**, the
package that registers every engine for the node and `viiwork-accept`.
[docs/adding-an-engine.md](docs/adding-an-engine.md) is the implementer's guide —
the five methods and what each must guarantee, the optional capabilities, and a
table of what the node already does so you write none of it. New engines run
`internal/engine/enginetest` against themselves for a pass/fail contract check.

## Models and hardware

On the gfx906 lane, one constraint shapes every deployment decision: **a dense
model whose weights plus KV cache do not fit in a single card pays roughly a 3×
throughput tax.** Above that line, tensor-split across several GPUs avoids the
tax at the cost of single-stream parallelism — layer split runs a group's cards
sequentially, so extra GPUs in a group buy VRAM and context, never throughput.

**The `freetoken` lane breaks that rule on purpose.** FreeToken runs one card per
process and never spreads a model across GPUs; instead it keeps only a sparse
MoE's hot experts in VRAM and streams the rest from host memory, so a checkpoint
far larger than the card runs on a *single* GPU. That is how the fleet serves
DeepSeek-V4-Flash on RTX 5090s. It costs load time — the default
`startup_timeout` for the engine is 30 minutes against llama.cpp's 10 — and it
comes with one trap worth reading before you plan one:
[large models on one card](docs/models.md#large-models-on-one-card-freetoken-and-moe-offload).

What the Radeon VII hosts serve today (the fleet also runs `vllm` on one host and
`freetoken` on two):

| Model | Spread | Context per slot | Role |
|---|---|---|---|
| `Qwen3.8-27B` | 2 hosts | 49152 | general coder and prose |
| `granite-4.2-8b` | 2 hosts | 16384 | fast utility model |
| `translategemma-27b-it` | 3 hosts, 3 backends each | 4096 | the translation lane |
| `gemma-4-31B-it` | 3 hosts | 6144 | prose |
| `Ornith-1.5-35B-A3B` | 1 host | 262144 | long context |

The full catalogue — measured throughput for every model benchmarked, the
validated production configs, the bring-ups that failed and why, and the gfx906
tuning rules that each cost a bring-up to learn — is
**[docs/models.md](docs/models.md)**.

## Security

**viiwork authenticates nothing.** Every endpoint is open to any client that can
reach it: reachability *is* the authorization model, and the fleet is expected to
sit on a tailnet. If you expose it more widely, put a reverse proxy or firewall
rules in front.

Prompt and response text is readable over the API, and an origin allowlist
(`api.cors`, defaulting to your own tailnet's `*.<tailnet>.ts.net`, learned
from tailscaled, and loopback) is what stops a page in some
browser on your network from quietly driving your fleet.
**[docs/security.md](docs/security.md)**.

## API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/models` | GET | Every model in the mesh (`owned_by` local, peer, pipeline or alias), with the context window a client can rely on |
| `/v1/chat/completions` | POST | Chat completion (routes by model; `?host=<node>` pins one machine) |
| `/v1/completions`, `/v1/embeddings` | POST | As above |
| `/health` | GET | Node health; 503 only when models are configured and none is healthy |
| `/v1/capacity` | GET | This node's slots, busy and queued per model |
| `/v1/status`, `/v1/cluster` | GET | This node's state; every member with its last status |
| `/v1/fleet/capacity` | GET | What the whole mesh can serve per model: slots, busy, queued and the context floor → [consuming it](docs/consuming-fleet-capacity.md) |
| `/api.json`, `/v1/model/info` | GET | The fleet described for coding clients (OpenCode, Roo Code) → [autodiscovery](docs/autodiscovery.md) |

Responses carry `X-Viiwork-Node` (the node that ran it), `X-Gpu-Backend` (for
example `Qwen3.8-27B/0`), `X-Viiwork-Model`, and where they apply
`X-Viiwork-Origin`, `X-Viiwork-Alias` and `X-Viiwork-Queued-Ms`.

The rest — aliases, metrics, activity and mesh streams, prompt lookup, power
control — plus request and response semantics, both integration modes and an
acceptance checklist: **[docs/api-integration.md](docs/api-integration.md)**.

## Builds

One image per engine, all under `docker/`. The Go binary is the same in every
one; what differs is the base image carrying the engine's runtime, so pinning
that base is how the inference stack gets pinned.

| Image | Dockerfile | Engine | Make target |
|---|---|---|---|
| `viiwork:latest` | `docker/Dockerfile.rocm` | `llamacpp` on ROCm / gfx906 | `make docker-rocm` (aliases `make docker`) |
| `viiwork-vllm:latest` | `docker/Dockerfile.vllm` | `vllm` | `make docker-vllm` |
| `viiwork-freetoken:latest` | `docker/Dockerfile.freetoken` | `freetoken` | `make docker-freetoken` |

`make docker` builds the ROCm image: Radeon VII is the core of this fleet, so the
unqualified target points there. Base image pins, CUDA GPU access through CDI,
the gfx906 FP8 patch and the retired fork track: **[BUILDS.md](BUILDS.md)**.

Tagged releases also ship static binaries for linux/amd64, linux/arm64 and
darwin/arm64, reproducibly built and signed: **[docs/releases.md](docs/releases.md)**.

## Documentation

| Document | What's in it |
|---|---|
| [setup.md](docs/setup.md) | `viiwork init`: the first-run wizard, what it writes, join codes, config-only cases |
| [configuration.md](docs/configuration.md) | Every config key: models, tensor-split, reloading, pipelines, GPU power limits, environment variables, host requirements |
| [mesh.md](docs/mesh.md) | Discovery, secured and open mode, routing and refusal handling, aliases |
| [dashboards.md](docs/dashboards.md) | `/`, `/mesh`, `/chat`, `/prompt`; how the live view is reconstructed; prompt and output history |
| [power-and-energy.md](docs/power-and-energy.md) | Fleet power, ENTSO-E cost tracking, the durable energy store, IPMI chassis control |
| [models.md](docs/models.md) | The measured catalogue: reference fleet, large models on one card via FreeToken offload, validated deployments, failed bring-ups, gfx906 tuning rules |
| [security.md](docs/security.md) | The trust model, what the lack of authentication exposes, CORS |
| [operations.md](docs/operations.md) | Scripts, `viiwork-accept` acceptance checks, the MCP server |
| [macos.md](docs/macos.md) | An Apple Silicon Mac as a native node: llama.cpp on Metal, sharing the one GPU, viiwork-parrot, Tailscale variants, launchd, sleep, uninstall |
| [api-integration.md](docs/api-integration.md) | Full API reference, both integration modes, semantics that bite, acceptance checklist |
| [autodiscovery.md](docs/autodiscovery.md) | Clients discovering the fleet: the enriched `/v1/models`, OpenCode's `/api.json` catalogue, LiteLLM's `/v1/model/info`, and what viiwork will not claim |
| [consuming-fleet-capacity.md](docs/consuming-fleet-capacity.md) | Building a client that reacts to fleet capacity rather than assuming it |
| [adding-an-engine.md](docs/adding-an-engine.md) | The engine contract: five methods, optional capabilities, packaging, testing |
| [thinking-models.md](docs/thinking-models.md) | Disabling thinking, diagnosing monologue leaking into `content`, capping reasoning |
| [energy-store-format.md](docs/energy-store-format.md) | The on-disk energy format, byte by byte, including reading it without Go |
| [tensor-split-design.md](docs/tensor-split-design.md) | Design record for multi-GPU backends |
| [migrating-to-v2.md](docs/migrating-to-v2.md) | v1 → v2: key mapping, a worked example, per-host conversion, rollback |
| [BUILDS.md](BUILDS.md) | The three images, base pins, GPU access, test images |

## Development

```bash
make build         # build binary (with git version embedded)
make mcp           # build MCP server
make test          # run unit tests
make docker        # build the ROCm image (viiwork:latest)

go test ./...                                   # unit tests
go test -tags=integration ./mesh/... ./internal/proxy/ ./internal/alias/ ./internal/node/
                                                # multi-node tests, in process, no GPU needed
go test -v -run TestName ./internal/package     # single test
go test -bench=. -benchmem ./internal/proxy ./internal/route   # hot-path benchmarks
```

Requires Go 1.27.1 (pinned in `go.mod` and the Dockerfiles). Dependencies are
`gopkg.in/yaml.v3`, `hashicorp/memberlist` (membership) and `hashicorp/mdns` (LAN
discovery); everything else is stdlib, deliberately.

The integration tests run whole nodes in one process on an in-memory network,
with the test binary standing in for `llama-server`, so they touch no GPU and no
real network. The benchmarks cover the per-token and per-request paths — SSE
response rewriting, request body parsing and route picking. Compare **allocation
counts** rather than wall-clock when judging a change: timings taken on a host
that is also serving models are extremely noisy, while alloc counts are
deterministic.
