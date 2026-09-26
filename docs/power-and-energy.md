# Power, Cost and Energy

Three related features, in increasing order of what they need from the machine:

| Feature | Answers | Needs |
|---|---|---|
| [Fleet power](#fleet-power) | What is the fleet drawing *now* | Nothing, or `/dev/ipmi0` for whole-chassis figures |
| [Cost tracking](#cost-tracking) | What is that costing *now* | An ENTSO-E API key |
| [Energy history](#energy-history) | Where did the kilowatt-hours *go* | A directory that outlives the container |
| [Power control](#power-control) | Switch a chassis on or off | An allowlist, and BMC credentials for powered-off hosts |

**Accuracy is ±15% on absolute watts** throughout. Compare models within a
machine freely, compare across machines with care, and never present any of it as
billing grade.

## Fleet power

The Fleet Power panel on [`/mesh`](dashboards.md#mesh-dashboard) needs no
configuration — it is drawn from the cluster snapshots the dashboard already
receives, so it adds no polling and no extra request. A machine appears in it as
soon as it can read its own power: from the BMC when the node has access to it,
else from the sum of its GPUs' board power.

```yaml
    devices:
      - /dev/ipmi0:/dev/ipmi0
```

Hosts without it are counted in the "n/m hosts reporting" line but contribute no
band, so a host that simply cannot be measured is never mistaken for a host
drawing nothing. The RAM strip below needs no BMC at all — it reads
`/proc/meminfo`, so every host appears in it.

Three things are worth knowing before reading numbers off it:

- **Wattage is per machine.** A BMC reading covers the whole chassis; a GPU sum
  covers only the cards. The table names which one each machine reports.
- **The window is since you opened the page**, capped at 720 readings. It is a
  live view, not history: a reload starts it over, and a halt leaves a gap rather
  than drawing a straight line across the pause. Durable per-host, per-model kWh
  is a separate feature — see [Energy history](#energy-history).
- **±15% on absolute watts.** Compare hosts and watch trends freely; do not bill
  anyone from it. The table names which source each host settled on (`dcmi`,
  `sdr`, `sensor:<name>`, or `rocm-smi`/`nvidia-smi` for a GPU sum).
- **A reading that stops refreshing goes absent, not flat.** When `ipmitool` or
  the GPU tool stops answering, the last good value counts for three sample
  periods (the observed `health.interval` tick, at least 5 s each) and is then
  reported unavailable until a read succeeds again. Status, cost and energy
  history all see "cannot say" for the outage rather than the last wattage
  repeated; a GPU whose samples stop drops out of `/v1/status` the same way.

If a host has the BMC device but still reports nothing, the probe found no source
that answers with a non-zero wattage — which is a real hardware answer, not a
bug: some boards expose the `Power Supply` sensor class as presence flags with no
watts. Startup logs name what was tried and what was adopted, and `power.source`
pins it if `auto` picks the wrong one:

```yaml
power:
  source: auto     # or dcmi | sdr | sensor:<NAME> | none
```

## Cost tracking

Real-time electricity cost per node, from ENTSO-E day-ahead spot prices.

1. Get an API key from the
   [ENTSO-E Transparency Platform](https://transparency.entsoe.eu/)
2. Put `ENTSOE_API_KEY=your-key-here` in the node's environment:
   `/etc/viiwork/mesh.env` with the compose example or the systemd unit, or a
   `.env` file in the directory the node runs from
3. Add a `cost` section to `viiwork.yaml` (see the example config) for your
   bidding zone and tariffs

The dashboards show per-node cost rate (EUR/h), daily accumulated cost and
cluster totals.

## Energy history

Cost tracking answers *what is this costing right now*. The energy store answers
*where did the kilowatt-hours go* — a durable per-host, per-model history of node
draw from IPMI and per-GPU draw from `rocm-smi`, kept for a year.

It is off by default, because it needs a directory that outlives the container:

```yaml
energy:
  enabled: true
  dir: /var/lib/viiwork/energy
  sample_interval: 30s   # 2x the BMC refresh; records are always 1/minute
```

```yaml
    devices:
      - /dev/ipmi0:/dev/ipmi0
    volumes:
      - /var/lib/viiwork/energy:/var/lib/viiwork/energy
```

Disk is fixed at creation and cannot grow: about 2.6 MB for a 10-GPU host, 660 KB
for two. Three preallocated ring files per series hold a day at one-minute
resolution, a year at one hour, and a year of daily totals; **retention is the
wrap**, so there is no purge job and a restart needs no recovery.

The node covers every GPU the machine reports, and takes each card's model from
its own `models[].gpus`, so one recorder produces a per-model split for the whole
box and a reload's layout is used at once. On a machine with no BMC, node power is
the sum of GPU board power (labelled `rocm-smi` or `nvidia-smi`) and every watt is
attributed to the cards directly.

Power is attributed **marginally**: each GPU is charged a share of node power in
proportion to how far it sits above its idle floor, and the baseline a host draws
just by being switched on (fans, CPU, idle cards, PSU losses) is reported
separately rather than smeared across models. Baseline plus every share equals
measured node power, so no total is invented.

Sampling is deliberately faster than recording. The BMC lags load and a
tensor-split pair's cards alternate between low and high draw, so only a mean over
the bucket is honest.

The on-disk format is a contract, specified byte by byte in
[energy-store-format.md](energy-store-format.md) — including how to read a store
without the Go package.

## Power control

Each host in the Fleet Power table can carry a power button. It is **off by
default** and there is no wildcard — only hosts you name can be targeted:

```yaml
power:
  control:
    enabled: true
    hosts: [node-a, node-b, node-c]
```

Hosts are node names. That much works immediately for hosts that are **running**:
the node on a machine controls it in-band through `/dev/ipmi0`, with no
credentials, and a request is forwarded to the member of that name.

A host that is **powered off** has no node to ask, so its BMC has to be reached
over the network. That needs credentials, and without them a host can be switched
off but not back on — the dashboard shows a disabled button saying so rather than
one that fails:

```yaml
    bmc:
      username: admin
      password_env: BMC_PASSWORD    # set in .env, not in the config file
      addresses:
        node-a: 192.0.2.65          # optional; see below
```

Addresses are optional per host. A node discovers its own BMC address in-band and
shares it, so a host seen online at least once needs no entry — which also means
a learned address cannot go stale the way a written one does when BMCs are on
DHCP.

The password is handed to `ipmitool` in `IPMI_PASSWORD` with `-E`, set on that
one command only, never as a `-P` argument: argv is readable by every process on
the host through `/proc/<pid>/cmdline`, and the containers run with `pid: host`.

Three things guard it, and **none of them is authentication** — viiwork has none,
and this does not add any:

- **The allowlist.** A host you did not name cannot be targeted, by the UI or by
  curl.
- **A node will not power off its own host.** Doing so would destroy the answer to
  the request and the dashboard asking it. Its button is disabled and the server
  refuses it too — open another node's `/mesh` to control that host.
- **A confirmation prompt** naming the host and the action.

Anyone who can reach the API can use it. That is the same trust model as the rest
of viiwork (see [security.md](security.md)), but the consequence is larger, so
keep the allowlist to the hosts you actually want reachable this way.
