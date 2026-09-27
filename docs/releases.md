# Releases

Every `v*` tag on the public repository gets release archives for linux/amd64,
linux/arm64 and darwin/arm64:

```
viiwork_vX.Y.Z_linux_amd64.tar.gz     viiwork, viiwork-accept, LICENSE
viiwork_vX.Y.Z_linux_arm64.tar.gz
viiwork_vX.Y.Z_darwin_arm64.tar.gz
SHA256SUMS                            sha256 of each archive
SHA256SUMS.sig                        ed25519 signature over the version and SHA256SUMS
```

CI builds the archives and uploads them with an **unsigned** `SHA256SUMS`. The
publisher then rebuilds the same version from their own checkout, checks that
every file matches CI's byte for byte, and only then signs. Nothing downloaded
is ever run on the machine that holds the key, so neither a compromised
workflow nor anyone who can move the public tag can get other code signed. The
signature covers the version, and only a `SHA256SUMS` listing exactly that
version's three archives is signed, so a signature can never be replayed for
another release. CI never holds the key, so a compromised
workflow can publish archives but cannot get them signed. A release without
`SHA256SUMS.sig` is not installable: nodes refuse it.

## Builds are reproducible

Every binary is built by `scripts/gobuild.sh` — `make`, CI and the local rebuild
alike — with `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and only
`-X main.version` in `-ldflags`. `scripts/build-release.sh` pins the compiler to
go.mod's exact version (`GOTOOLCHAIN`) and writes deterministic archives.
`bash scripts/build-release_test.sh` builds the committed tree twice from two
directories and checks that every byte agrees.

## Publishing a release

1. Cut the release as usual (`CHANGELOG.md` heading, `scripts/publish.sh --tag vX.Y.Z …`).
   The tag push starts the `release` workflow on the public repository.
2. When the workflow has finished, on the publisher's machine:

   ```sh
   scripts/release-sign.sh vX.Y.Z [COMMIT]
   ```

   Run it in the private repository at the commit that was published (`HEAD`
   by default). It downloads the archives, rebuilds that commit, runs
   `viiwork-release compare` on every archive, signs `SHA256SUMS` for the
   version, checks the signature against that commit's compiled-in keys, and
   attaches `SHA256SUMS.sig`. `bash scripts/release-sign_test.sh` exercises it
   end to end against a stand-in `gh`.

   The workflow refuses to re-upload archives once a release carries a
   signature, and only its `publish` job, which runs nothing but `gh`, holds
   write access.

3. `release-sign.sh` then publishes the engine images, from the same local
   build it just compared: `ghcr.io/janit/viiwork-llamacpp-cuda` (amd64,
   arm64), `viiwork-llamacpp-vulkan` and `viiwork-vllm`, each tagged with the
   version. Each is the upstream engine image (pins in `docker/pins.env`) with
   `viiwork` appended as one layer, streamed registry-to-registry by `crane`.
   Its `gh` token needs the `write:packages` scope (`gh auth refresh -s
   write:packages`). A new package on ghcr is private until made public once
   in its settings. gfx906 (Radeon VII) has no published image: `make docker`
   builds it. `SKIP_IMAGES=1` signs without publishing. Every image records
   its base by digest (`org.opencontainers.image.base.digest`), and a released
   tag is written once. If a push fails after the signature is attached,
   `IMAGES_ONLY=1 scripts/release-sign.sh vX.Y.Z` rebuilds, compares, verifies
   the existing signature and publishes only the images. The registry login
   lives in the run's temporary directory and goes with it.

   The base images keep their own HEALTHCHECK (llama.cpp's probes :8080); a
   compose file sets its own.

viiwork-parrot follows the releases feed and seeds each release as a torrent
once its signature is attached.

## The signing key

`go run ./cmd/viiwork-release keygen` makes the pair once, on the publisher's
machine: the private key at `~/.config/viiwork/release.key` (mode 0600; `sign`
refuses a key others can read) and the public key at
`internal/release/keys/release.pub`, which is committed and compiled into every
build. `keygen` refuses to overwrite an existing key.

Back the private key up offline. Losing it, or suspecting it has leaked, means a
rotation: make a new pair under a new file name (`--pub
internal/release/keys/<year>.pub`), ship a release carrying both public keys,
sign later releases with the new key, and remove the old public key once no
node runs a version that lacks the new one.

## Checking a download by hand

```sh
sha256sum --check --ignore-missing SHA256SUMS
go run ./cmd/viiwork-release verify --version vX.Y.Z SHA256SUMS SHA256SUMS.sig   # from a checkout
```

## How a node updates itself

A node takes part only with `update.enabled: true`. Everything lives under
`<state_dir>/releases/`: `state.json` (`current`, `last_good`, `pending`, the
recorded launcher) and one directory per staged version.

- **Stage** (`POST /v1/update/stage {"version": "vX.Y.Z"}`): the node downloads
  `SHA256SUMS`, its signature and this host's archive from
  `<update.source>/vX.Y.Z/`, verifies the signature for that version against
  the compiled-in keys, checks the archive, unpacks `viiwork` and
  `viiwork-accept`, runs the new binary with `--version` (it must run here and
  report that version) and `--engine-requirements` (this host's engines must be
  new enough). Anything that fails leaves nothing behind.
- **Activate** (`POST /v1/update/activate`): records the backends healthy right
  now as the baseline, marks the version pending, answers 202, shuts down in
  the usual order and execs the launcher, which runs the staged release in the
  same process.
- **Confirm**: the new release becomes last good once every baseline backend is
  healthy again. A baseline backend going dead, or the window passing
  (`update.confirm_timeout`, else the sum of the models' startup timeouts plus
  10 minutes, fetching time excluded), rolls it back. A release that crashes
  before it can confirm gets three starts.
- **Rollback** (`POST /v1/update/rollback`): back to last good at once.
- A newer image or binary installed out of band always wins over an older
  staged release.

Writes need a meshauth signature in a secured mesh and come only from loopback
in an open one, exactly like alias writes. `GET /v1/update` reports the state,
the staged versions and the installed engine versions.

## Rolling a release across the mesh

```sh
viiwork update status                        # every member: running, current, last good, pending, engines
viiwork update                               # the latest signed release, everywhere it is enabled
viiwork update --to v2.6.0 --hosts gb1,gb2   # one version, some hosts
viiwork update rollback --host gb2           # one host back to its last good release
```

A rollout lists its plan — every member with what will happen to it, or why it
is skipped (not alive, updates off, already there, newer, older than rolling
updates) — and asks for `YES I WANT TO UPDATE ALL NODES` (or `--confirm`). It
then stages the release on every chosen host, so a download, signature or
engine problem stops everything before any host restarts, and activates them
one at a time, the node it talks to last. Each host must come back on the new
release and confirm it before the next starts; a host that rolls itself back,
or misses its own confirmation deadline, stops the rollout with the rest
untouched. In a secured mesh, run it where the mesh secret is loaded (`set -a;
. /etc/viiwork/mesh.env; set +a`, or inside a node's container); without it the
CLI says so before asking for anything. An open mesh accepts update writes only
from each node's own machine, so there a rollout updates only the node the CLI
talks to — run it on that machine against `127.0.0.1` — and lists the others as
skipped. A version below a host's installed binary is skipped too: the node's
newer-binary rule would undo it at the next start. Names in `--hosts` that are
not members are refused.
