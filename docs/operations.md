# Operations

Running a fleet: the scripts, the acceptance checker and the MCP server.

- [Scripts](#scripts)
- [Acceptance checks](#acceptance-checks)
- [MCP server](#mcp-server)

## Scripts

Deploying and updating:

| Script | Description |
|--------|-------------|
| `scripts/update.sh` | Pull latest, rebuild the image, restart, wait on `:8086/health` |
| `scripts/rebuild.sh` | Full clean rebuild: stop, remove images, rebuild, start, wait on `:8086/health`. Never prunes volumes — a node's state directory must survive a routine rebuild |
| `scripts/version.sh` | The single source of the version stamped into a build. An exact git tag wins; otherwise it reads `CHANGELOG.md`'s top heading, because builds do not rely on tags |
| `scripts/verify-environment.sh` | Check a host's drivers, devices and ports before a bring-up |

Benchmarking and tuning:

| Script | Description |
|--------|-------------|
| `scripts/bench.sh` | Stress benchmark: ramp concurrency from 1 to N, measure throughput and latency |
| `scripts/bench-sustained.sh` | Sustained load: hold N concurrent requests for a duration |
| `scripts/power-perf-sweep.sh` | Sweep one GPU through power caps (150/180/210/250 W), measure tok/s + watts + temperature, recommend a `power_limit_watts`. ~15-20 min, power-cap-only, fully reversible |
| `scripts/power-perf-sweep-phase2.sh` | Advanced sweep: voltage curve and memory clock tuning. Riskier than phase 1 — requires explicit go-ahead. Has a correctness gate that compares outputs against baseline |

Clients and models:

| Script | Description |
|--------|-------------|
| `scripts/setup-opencode.sh` | Configure an OpenCode client with auto-detected models |
| `scripts/download-*.sh` | Fetch a specific model's GGUFs with the quant and repo the fleet settled on |
| `scripts/fetch-engine.py` | Fetch an inference engine's release artifacts |

`setup-node.sh` was removed in v2.1.0: it wrote viiwork 1.x layouts, and its
first prompt offered the retired gfx906 fork image. Set a v2 node up by copying
`configs/docker-compose.v2.example.yaml` and `viiwork.yaml.example`, and convert
a v1 host with [migrating-to-v2.md](migrating-to-v2.md). `deploy.sh`, which
drove the one-instance-per-model v1 layouts, left the published tree with those
layouts.

## Acceptance checks

`viiwork-accept` checks a node and a mesh **without changing either**. It reads
state and sends inference requests; every lifecycle action stays the operator's,
which is what makes it safe to point at a live fleet.

```bash
make accept                                  # builds bin/viiwork-accept

# Before a machine is converted, while the old setup is still serving:
viiwork-accept config --file viiwork.yaml --dummy-secret --models-root /srv/models

# Is the gossip port actually reachable between two machines?
viiwork-accept ports serve --addr 198.51.100.10      # on the machine
viiwork-accept ports probe --host node-a             # from another one

# After starting it:
viiwork-accept ready  --node node-a:8086 --timeout 50m
viiwork-accept models --node node-a:8086 --via node-b:8086
viiwork-accept saturate --node node-a:8086 --model some-model-27B
viiwork-accept alias  --entry node-a:8086 --alias stable-coder --expect-model some-model-27B

# Timings in and out of the mesh, observed from another node:
viiwork-accept join --observer node-b:8086 --node node-a --expect some-model-27B
viiwork-accept gone --observer node-b:8086 --node node-a --expect-state left
```

Exit code 0 when every check passed, 1 when one failed, 2 for a usage error, so
it drops into a script unchanged. `--json` writes the report as JSON.

Two behaviours are worth knowing before you read a report:

- **`config` enforces the fleet convention**, so a node deliberately on a
  non-standard `api.port` or `mesh.bind_port` fails those two checks while being
  otherwise valid.
- **`ready` waits per model**, that is until each has one healthy backend and can
  serve. Its final "all backends healthy" check is a single look, so on a machine
  whose backends load one after another it can report a failure while loading is
  still progressing normally. Re-run it once loading settles.

The per-host conversion procedure that strings these together is section 7 of
[migrating-to-v2.md](migrating-to-v2.md).

## MCP server

`viiwork-mcp` exposes the viiwork cluster as tools for any MCP-compatible AI
assistant, letting coding tools delegate inference to your locally hosted models.

```bash
make mcp                                          # builds bin/viiwork-mcp
viiwork-mcp --url http://your-viiwork-host:8086   # or set VIIWORK_URL
```

| Tool | Description |
|------|-------------|
| `query` | Send a prompt to a model. Params: `prompt` (required), `system`, `model`, `max_tokens`, `temperature` |
| `models` | List available models on the cluster |
| `status` | Cluster health, per-GPU backend status, in-flight counts |

Add it to your MCP client's configuration as a stdio transport server pointing at
the `viiwork-mcp` binary.
