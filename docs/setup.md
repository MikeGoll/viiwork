# Setting up a machine

`viiwork init` turns a machine with GPUs and some GGUF model files into a
running node. It asks a few questions and shows every file it will write. It
writes nothing until you say yes, then starts the node and checks it. Running
`viiwork` on a terminal with no config file starts the same wizard.

On Linux the engine runs in Docker. On an Apple Silicon Mac it runs natively
under a user LaunchAgent: see [On a Mac](#on-a-mac).

## Before you start

- A Linux machine with an NVIDIA GPU, Docker Engine and the
  [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/).
  The setup wizard checks all three.
- GGUF model files in a directory (`/models`, `/srv/models` or `~/models` are
  suggested when they exist). The wizard does not download models.
- Ports 8086/tcp (the API and dashboards) and 7946/tcp+udp (the mesh) free.

## Download and verify

Pick the archive for your machine from the
[releases](https://github.com/janit/viiwork/releases), then check it against
the signed `SHA256SUMS` ([releases.md](releases.md) explains the signature):

```sh
v=vX.Y.Z
base=https://github.com/janit/viiwork/releases/download/$v
curl -fLO "$base/viiwork_${v}_linux_amd64.tar.gz" -fLO "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
tar xzf "viiwork_${v}_linux_amd64.tar.gz"
sudo ./viiwork init
```

## What it asks

1. **Preflight.** The wizard needs root (it writes `/etc/viiwork`), Docker, and
   the ports free. It stops if a config or an earlier install is already there,
   or if a container named `viiwork` exists: a node set up by hand is left
   alone.
2. **Hardware.** It lists the GPUs and names the engine image it will run.
3. **Models.** It asks for your models directory, lists each model it finds
   with its architecture and size, and asks which to serve and what to call
   them. Second shards of a split model and vision projectors are not listed.
4. **Layout.** It proposes which cards each model gets, the context per slot,
   and how many slots. You can accept the layout, edit one model's line, or
   drop a model. A layout the node would refuse is shown with the reason.
5. **Mesh.** First it looks for nodes nearby, on your tailnet and on the LAN.
   Then it offers three choices:
   - join an existing mesh with a **join code** (see below);
   - start a new secured mesh;
   - start an open mesh (no secret), with a warning.
6. **Updates.** Choose whether this machine accepts mesh-wide updates
   (`viiwork update`). Only a secured mesh can have them.
7. **Node name.** The default comes from the host name.
8. **Review.** It shows every file with its content, the secret masked. On a
   yes it writes the files, pulls the image, starts the node, and waits until
   every model has loaded.
9. **Finish.** It runs the `viiwork-accept` config and ready checks, plus the
   join check when the node joined a mesh. Then it prints how to reload and
   restart. A new secured mesh also gets the join code for the next machine.

Pressing Ctrl-C or Ctrl-D at any question leaves nothing behind.

## What it writes

| Path | Mode | What |
|---|---|---|
| `/etc/viiwork/viiwork.yaml` | 0644 | The config. Edit it freely, then `docker kill -s HUP viiwork`. |
| `/etc/viiwork/mesh.env` | 0640 | The mesh secret (secured mesh only). |
| `/etc/viiwork/docker-compose.yaml` | 0644 | One service: host networking, the GPUs, the models directory read-only at `/models`, `restart: always`. |
| `/etc/viiwork/install.json` | 0644 | The manifest: exactly what `viiwork uninstall` removes. |
| `/usr/local/bin/viiwork` | 0755 | A copy of the binary. |
| `/var/lib/viiwork/` | — | Node state: the alias table and staged releases. |

Model weights are never copied or moved.

## When it writes the config only

Only some machines get an engine image and a started node. In these cases the
wizard writes the config and the manifest, starts nothing, and says why:

- **A development build.** Only a release (`vX.Y.Z`, or a pre-release such as `v2.6.0-beta1`) has
  published images.
- **An AMD Radeon VII or MI50 (gfx906).** It has no published image. Build one
  from a checkout with `make docker`, and run it with
  `configs/docker-compose.v2.example.yaml`.
- **Other AMD cards.** No image is verified yet. viiwork pins a backend to its
  cards with `ROCR_VISIBLE_DEVICES`, which the Vulkan build ignores.

The config's model paths are `/models/...`: mount your models directory there.

## Joining the next machine

On any node of a secured mesh, run:

```sh
sudo sh -c 'set -a; . /etc/viiwork/mesh.env; viiwork join-code'
```

This prints a join code starting with `viiwork1-`. It **is** the mesh secret:
treat it like `mesh.env`, and never post it anywhere. Run `sudo ./viiwork init`
on the new machine and paste the code at the mesh question. In an open mesh,
`viiwork join-code --open` prints a code without a secret. On a Mac set up by
`viiwork init`, the secret is only in the LaunchAgent's plist, and a plain
`viiwork join-code` (no sudo) reads it from there.

## When the node does not come up

The files are kept. The wizard prints each backend that is not healthy, with
its status and phase, followed by the container's last log lines. The most
common cause is a model that does not fit in memory at the chosen context: edit
the `context` or `gpus` in `viiwork.yaml`, then retry:

```sh
sudo docker compose -f /etc/viiwork/docker-compose.yaml -p viiwork up -d
```

To start over, `sudo viiwork uninstall` removes everything in the manifest and
nothing else.

## Uninstalling

```sh
sudo viiwork uninstall [--yes] [--delete-models] [--keep-images]
```

Uninstall reads `/etc/viiwork/install.json` and lists everything it will
remove, with sizes. Then it asks you to type `uninstall`. If this node is the
mesh's last member, it says so first: the alias table, and in a secured mesh
the only copy of the secret, go with it. After you confirm, it:

1. stops the node the normal way, so it leaves the mesh and in-flight requests
   finish;
2. runs `docker compose down` for its own project only;
3. removes the engine image, unless you pass `--keep-images` (or another
   container still uses it);
4. removes the files, the state directory and the binary.

It never prunes, never touches another compose project, and leaves any file
you added to `/etc/viiwork`.

**Model weights are kept**, so a reinstall downloads nothing.
`--delete-models` also deletes the model files this node served. It lists them
first and asks for its own confirmation, and it never deletes anything else in
the models directory.

**A host set up by hand** has no manifest, so uninstall refuses rather than
guess. `viiwork uninstall --from-config` lists what the config points at and
removes nothing.

## On a Mac

On an Apple Silicon Mac, run `./viiwork init` **without sudo**. The wizard asks
the same questions, with two differences:

- **Planning.** The Mac has one GPU, whose memory it shares with everything
  else on the machine. Models are planned against the Metal budget
  (`iogpu.wired_limit_mb`, about 75% of RAM by default), minus headroom. A
  model that does not fit is shown with the reason.
- **What gets installed.** No Docker. Instead the wizard fetches llama.cpp at
  the release this binary was built against. It checks the download against
  the sha256 that GitHub publishes for it, then installs a LaunchAgent.

| Path | Mode | What |
|---|---|---|
| `~/.config/viiwork/viiwork.yaml` | 0644 | The config: absolute paths, `gpu.vendor: apple`, each model's `llamacpp.binary`. |
| `~/Library/LaunchAgents/fi.viiwork.node.plist` | 0600 | The agent, with the mesh secret in its environment. |
| `~/.local/share/viiwork/llama.cpp/<tag>/` | — | The llama.cpp build, quarantine attribute cleared. A build already there (from `scripts/macos/fetch-llama.sh`) is reused and left alone by uninstall. |
| `~/.local/bin/viiwork` | 0755 | A copy of the binary. |
| `~/.local/state/viiwork/`, `~/Library/Logs/viiwork/` | — | Node state, and the log. |
| `~/.config/viiwork/install.json` | 0644 | The manifest. |

After setup:

- Reload the config with `launchctl kill HUP gui/$(id -u)/fi.viiwork.node`.
- Restart the node with `launchctl kickstart -k gui/$(id -u)/fi.viiwork.node`.
- A sleeping Mac leaves the mesh, so run `caffeinate -s` while it serves on
  power.
- Remove the install with `viiwork uninstall`, without sudo.
- [macos.md](macos.md) covers the rest: Tailscale builds, memory, and what
  reads as unavailable.
