#!/usr/bin/env bash
# Publishes the vulkan flavour of a throwaway release into a local registry,
# then pulls and runs it:
#
#	bash scripts/release-images_test.sh
#
# Needs Docker and network access to ghcr.io (the base image is streamed from
# there). The registry listens on 127.0.0.1 only and is removed afterwards.
set -euo pipefail
cd "$(dirname "$0")/.."
. docker/pins.env
version=v0.0.0-imgtest
reg=127.0.0.1:5056
tmp=$(mktemp -d)
docker run -d --rm --name viiwork-imgtest-registry -p "$reg:5000" registry:2 >/dev/null
cleanup() {
	docker rm -f viiwork-imgtest-registry >/dev/null 2>&1 || true
	docker rmi -f "$reg/viiwork-llamacpp-vulkan:$version" "$reg/viiwork-llamacpp-cuda:$version" >/dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap cleanup EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

./scripts/build-release.sh "$version" "$tmp/build" >/dev/null

# A pin with no upstream image fails before anything is pushed.
if FLAVOURS=llamacpp-vulkan LLAMA_CPP_IMAGE_BUILD=b1 CRANE_FLAGS=--insecure \
	./scripts/release-images.sh "$version" "$tmp/build" "$reg" >"$tmp/out" 2>&1; then
	bad "a missing base tag was published"
elif grep -q "server-vulkan-b1" "$tmp/out"; then
	ok "a missing base tag is refused and named"
else
	bad "missing base: $(tail -1 "$tmp/out")"
fi

FLAVOURS="llamacpp-vulkan llamacpp-cuda" CRANE_FLAGS=--insecure \
	./scripts/release-images.sh "$version" "$tmp/build" "$reg" >"$tmp/out" 2>&1 || { bad "publish: $(tail -3 "$tmp/out")"; exit 1; }
grep -q "$reg/viiwork-llamacpp-vulkan:$version" "$tmp/out" && ok "the pushed reference is printed" || bad "no pushed reference printed"

img="$reg/viiwork-llamacpp-vulkan:$version"
docker pull -q "$img" >/dev/null
[ "$(docker run --rm "$img" --version)" = "$version" ] && ok "viiwork --version" || bad "viiwork --version"
docker run --rm --entrypoint sh "$img" -c 'command -v llama-server' | grep -q /app/llama-server && ok "llama-server on PATH" || bad "llama-server not on PATH"
label=$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.version"}}' "$img")
[ "$label" = "$version" ] && ok "version label" || bad "version label: $label"
cmd=$(docker inspect -f '{{json .Config.Cmd}}' "$img")
[ "$cmd" = '["--config","/etc/viiwork/viiwork.yaml"]' ] && ok "cmd" || bad "cmd: $cmd"

# Each platform carries its own target's binary.
# Unquoted on purpose, as in release-images.sh: the default is a command line.
crane=${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.22.1}
arm=$($crane --insecure \
	export --platform linux/arm64 "$reg/viiwork-llamacpp-cuda:$version" - | tar -xOf - usr/local/bin/viiwork | sha256sum | cut -d' ' -f1)
want=$(sha256sum "$tmp/build/viiwork_${version}_linux_arm64/viiwork" | cut -d' ' -f1)
[ "$arm" = "$want" ] && ok "the arm64 image carries the arm64 binary" || bad "arm64 image binary $arm, want $want"
# The base is recorded by digest, not only by its mutable tag.
digest=$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.base.digest"}}' "$img")
case $digest in sha256:*) ok "base digest label" ;; *) bad "base digest label: $digest" ;; esac

# A published tag is written once: a second publish of the same version is
# refused before anything is pushed, unless REPUBLISH=1.
if FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure ./scripts/release-images.sh "$version" "$tmp/build" "$reg" >"$tmp/out" 2>&1; then
	bad "an existing tag was overwritten"
elif grep -q "already published" "$tmp/out"; then
	ok "an existing tag is refused"
else
	bad "existing tag: $(tail -1 "$tmp/out")"
fi

# The layer's directories and binary are world-readable whatever the
# publisher's umask: they replace the base's /usr metadata in the image.
(umask 077 && FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure REPUBLISH=1 \
	./scripts/release-images.sh "$version" "$tmp/build" "$reg" >"$tmp/out" 2>&1) || bad "republish: $(tail -2 "$tmp/out")"
docker rmi -f "$img" >/dev/null 2>&1
docker pull -q "$img" >/dev/null
modes=$(docker run --rm --entrypoint stat "$img" -c '%a' /usr/local/bin/viiwork /usr/local/bin /usr/local | tr '\n' ' ')
[ "$modes" = "755 755 755 " ] && ok "modes under umask 077" || bad "modes under umask 077: $modes"

# A multi-gigabyte stream can drop (a TLS error from the base registry was
# seen mid-copy): one failed push is retried, not fatal.
real=${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.22.1}
cat >"$tmp/flaky-crane" <<EOF
#!/usr/bin/env bash
case " \$* " in *" mutate "*) mutate=1 ;; *) mutate= ;; esac
if [ -n "\$mutate" ] && [ ! -e "$tmp/failed-once" ]; then
	touch "$tmp/failed-once"
	echo "simulated: tls: bad record MAC" >&2
	exit 1
fi
exec $real "\$@"
EOF
chmod +x "$tmp/flaky-crane"
if FLAVOURS=llamacpp-vulkan CRANE="$tmp/flaky-crane" CRANE_FLAGS=--insecure RETRY_WAIT=1 REPUBLISH=1 \
	./scripts/release-images.sh "$version" "$tmp/build" "$reg" >"$tmp/out" 2>&1 && [ -e "$tmp/failed-once" ]; then
	ok "a dropped push is retried"
else
	bad "a dropped push: $(tail -2 "$tmp/out")"
fi
exit $fail
