#!/usr/bin/env sh
# Fetch the prebuilt llama.cpp for Apple Silicon (Metal) and print the path of
# its llama-server, for models[].llamacpp.binary in a Mac's viiwork.yaml.
#
#   scripts/macos/fetch-llama.sh [tag] [dir]
#
# The tag defaults to the fleet's pin, ARG LLAMA_CPP_VERSION in
# docker/Dockerfile.rocm, so a Mac runs the same llama.cpp release as the
# Linux nodes and the pin has one home. Each tag unpacks into its own
# directory under dir (default ~/.local/share/viiwork/llama.cpp), so moving the
# pin never overwrites the build a running node uses. Not Homebrew: its formula
# moves daily and is not pinned to the fleet's release.
#
# The pin's download is checked against LLAMA_CPP_MACOS_SHA256 in
# docker/pins.env; another tag is not checked.
#
# Re-running with a tag already present prints its path and downloads nothing.
set -eu

here=$(cd "$(dirname "$0")/../.." && pwd)
tag=${1:-$(sed -n 's/^ARG LLAMA_CPP_VERSION=//p' "$here/docker/Dockerfile.rocm" | head -1)}
root=${2:-$HOME/.local/share/viiwork/llama.cpp}
if [ -z "$tag" ]; then
	echo "no tag given and none found in docker/Dockerfile.rocm" >&2
	exit 1
fi
case $(uname -s)/$(uname -m) in
Darwin/arm64) ;;
*) echo "this fetches the macOS arm64 build; run it on an Apple Silicon Mac" >&2; exit 1 ;;
esac

dest=$root/$tag
find_server() { find "$dest" -name llama-server -type f -perm -u+x 2>/dev/null | head -1; }

server=$(find_server)
if [ -n "$server" ]; then
	echo "$server"
	exit 0
fi

asset=llama-$tag-bin-macos-arm64.tar.gz
url=https://github.com/ggml-org/llama.cpp/releases/download/$tag/$asset
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "fetching $url" >&2
curl -fL --proto '=https' --retry 3 -o "$tmp/$asset" "$url"
pins=$here/docker/pins.env
if [ -f "$pins" ] && [ "$(sed -n 's/^LLAMA_CPP_VERSION=//p' "$pins")" = "$tag" ]; then
	want=$(sed -n 's/^LLAMA_CPP_MACOS_SHA256=//p' "$pins")
	got=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
	if [ "$got" != "$want" ]; then
		echo "$asset: sha256 $got, but docker/pins.env pins $want" >&2
		exit 1
	fi
fi
mkdir -p "$tmp/unpack"
tar -xzf "$tmp/$asset" -C "$tmp/unpack"
# A downloaded binary carries the quarantine attribute, and Gatekeeper refuses
# to start an unsigned one that has it.
xattr -dr com.apple.quarantine "$tmp/unpack" 2>/dev/null || true

mkdir -p "$root"
mv "$tmp/unpack" "$dest"
server=$(find_server)
if [ -z "$server" ]; then
	echo "$asset unpacked into $dest but holds no llama-server" >&2
	exit 1
fi
"$server" --version >&2
echo "$server"
