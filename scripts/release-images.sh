#!/usr/bin/env bash
# Publish a release's engine images: each upstream engine image plus this
# release's viiwork as one extra layer, pushed as REGISTRY/viiwork-<flavour>:VERSION.
#
#	scripts/release-images.sh VERSION BUILDDIR REGISTRY
#
# BUILDDIR is a scripts/build-release.sh output directory. release-sign.sh runs
# this after the byte-for-byte compare, so an image carries exactly the bytes
# the publisher verified; CI never pushes one. The base is streamed
# registry-to-registry by crane — nothing multi-gigabyte lands on disk.
#
# FLAVOURS (default: llamacpp-cuda llamacpp-vulkan vllm), CRANE (default: a
# pinned `go run`), CRANE_FLAGS (e.g. --insecure for a local test registry).
set -euo pipefail
cd "$(dirname "$0")/.."
if [ $# -ne 3 ]; then
	echo "usage: release-images.sh VERSION BUILDDIR REGISTRY" >&2
	exit 2
fi
version=$1
build=$2
registry=${3%/}
pins_image_build=${LLAMA_CPP_IMAGE_BUILD:-}
vllm_version=${VLLM_VERSION:-}
. docker/pins.env
# An explicit environment value wins over pins.env (the test uses it).
LLAMA_CPP_IMAGE_BUILD=${pins_image_build:-$LLAMA_CPP_IMAGE_BUILD}
VLLM_VERSION=${vllm_version:-$VLLM_VERSION}
crane=${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.22.1}
flags=${CRANE_FLAGS:-}
flavours=${FLAVOURS:-llamacpp-cuda llamacpp-vulkan vllm}
std_path=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# flavour NAME -> "BASE|PLATFORMS|PATH"
flavour() {
	case $1 in
	llamacpp-cuda) echo "ghcr.io/ggml-org/llama.cpp:server-cuda-$LLAMA_CPP_IMAGE_BUILD|linux/amd64 linux/arm64|/app:$std_path" ;;
	llamacpp-vulkan) echo "ghcr.io/ggml-org/llama.cpp:server-vulkan-$LLAMA_CPP_IMAGE_BUILD|linux/amd64|/app:$std_path" ;;
	vllm) echo "vllm/vllm-openai:$VLLM_VERSION|linux/amd64|/usr/local/cuda/bin:$std_path" ;;
	*) return 1 ;;
	esac
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# retry runs one push up to three times: streaming a multi-gigabyte base from
# one registry to another can drop mid-copy, and a publish from a home uplink
# should not have to start over for it.
retry() {
	local n
	for n in 1 2 3; do
		if "$@"; then
			return 0
		fi
		if [ "$n" -lt 3 ]; then
			echo "release-images.sh: attempt $n failed; retrying" >&2
			sleep "${RETRY_WAIT:-10}"
		fi
	done
	return 1
}

# Check everything before anything is pushed: every base exists with every
# platform this flavour ships, and no version tag is published yet. Each base
# is resolved once to a digest; that digest, not the mutable tag, is what the
# image is built on and what its label records.
declare -A base_digest
for f in $flavours; do
	spec=$(flavour "$f") || { echo "release-images.sh: unknown flavour $f" >&2; exit 2; }
	base=${spec%%|*}
	rest=${spec#*|}
	platforms=${rest%%|*}
	if ! d=$($crane $flags digest "$base" 2>"$work/err"); then
		echo "release-images.sh: base image $base: $(tail -1 "$work/err")" >&2
		exit 1
	fi
	base_digest[$f]=$d
	manifest=$($crane $flags manifest "${base%:*}@$d")
	for p in $platforms; do
		arch=${p#linux/}
		if ! grep -q "\"architecture\": *\"$arch\"" <<<"$manifest"; then
			echo "release-images.sh: base image $base has no $p" >&2
			exit 1
		fi
	done
	target="$registry/viiwork-$f:$version"
	if $crane $flags digest "$target" >/dev/null 2>&1 && [ "${REPUBLISH:-}" != 1 ]; then
		echo "release-images.sh: $target is already published; a released tag is written once (REPUBLISH=1 to replace it)" >&2
		exit 1
	fi
done

for f in $flavours; do
	spec=$(flavour "$f")
	base=${spec%%|*}
	rest=${spec#*|}
	platforms=${rest%%|*}
	path=${rest#*|}
	pinned="${base%:*}@${base_digest[$f]}"
	target="$registry/viiwork-$f:$version"
	refs=()
	for p in $platforms; do
		arch=${p#linux/}
		bin="$build/viiwork_${version}_linux_${arch}/viiwork"
		[ -x "$bin" ] || { echo "release-images.sh: no $bin in the build directory" >&2; exit 1; }
		root="$work/$f-$arch/root"
		mkdir -p "$root/usr/local/bin"
		cp "$bin" "$root/usr/local/bin/viiwork"
		# These entries replace the base's metadata for /usr, /usr/local and
		# /usr/local/bin, so their modes are fixed, not the publisher's umask.
		tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u=rwX,go=rX \
			-C "$root" -cf "$work/$f-$arch.tar" usr
		# Pushed by digest per platform, then joined into one index below.
		ref=$(retry $crane $flags mutate "$pinned" --platform "$p" --append "$work/$f-$arch.tar" \
			--entrypoint viiwork --cmd --config,/etc/viiwork/viiwork.yaml \
			-e "PATH=$path" \
			-l org.opencontainers.image.source=https://github.com/janit/viiwork \
			-l "org.opencontainers.image.version=$version" \
			-l "org.opencontainers.image.base.name=$base" \
			-l "org.opencontainers.image.base.digest=${base_digest[$f]}" \
			--repo "$registry/viiwork-$f")
		echo "pushed $p: $ref"
		refs+=("$ref")
	done
	if [ ${#refs[@]} -eq 1 ]; then
		retry $crane $flags copy "${refs[0]}" "$target" >/dev/null
	else
		args=()
		for r in "${refs[@]}"; do args+=(-m "$r"); done
		retry $crane $flags index append "${args[@]}" -t "$target" >/dev/null
	fi
	echo "pushed $target"
done
