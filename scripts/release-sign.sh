#!/usr/bin/env bash
# Sign a release CI built, after proving it: download its archives, rebuild the
# same version here from the publisher's OWN checkout, compare every file byte
# for byte, and only then sign SHA256SUMS and attach the signature.
#
#	scripts/release-sign.sh VERSION [COMMIT]
#
# COMMIT (default HEAD) is a commit of the repository this script lives in —
# the trusted source the public tag was published from. Nothing downloaded is
# ever executed: the rebuild and the signing tool both come from COMMIT, so
# neither a compromised workflow nor anyone who can move the public tag can get
# other code signed, or run code on the machine that holds the key. The Go
# sources are identical in the private and public trees and builds are
# reproducible (-trimpath), so a genuine release matches byte for byte and
# anything else fails closed. docs/releases.md.
#
# RELEASE_REPO (default janit/viiwork), VIIWORK_RELEASE_KEY (default
# ~/.config/viiwork/release.key), RESIGN=1 to replace an existing signature,
# RELEASE_SIGN_PUB to check the new signature against that public key file
# instead of the keys compiled into COMMIT, IMAGE_REGISTRY (default
# ghcr.io/janit) for the engine images, SKIP_IMAGES=1 to publish none,
# IMAGES_ONLY=1 to publish the images of a release that is already signed —
# the recovery when a push failed after signing: it rebuilds and compares as
# usual and verifies the existing signature, but signs and uploads nothing.
# IMAGE_LOGIN=1 forces the registry login for a registry other than ghcr.io.
set -euo pipefail
if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: release-sign.sh VERSION [COMMIT]" >&2
	exit 2
fi
tag=$1
repo=${RELEASE_REPO:-janit/viiwork}
key=${VIIWORK_RELEASE_KEY:-$HOME/.config/viiwork/release.key}
if ! printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; then
	echo "release-sign.sh: not a release version: $tag" >&2
	exit 2
fi
for cmd in gh git go sha256sum tar; do
	command -v "$cmd" >/dev/null || { echo "release-sign.sh: $cmd is required" >&2; exit 1; }
done
[ -f "$key" ] || { echo "release-sign.sh: no signing key at $key (viiwork-release keygen)" >&2; exit 1; }

src_repo=$(cd "$(dirname "$0")/.." && pwd)
commit=$(git -C "$src_repo" rev-parse --verify "${2:-HEAD}^{commit}")

assets=$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name')
if grep -qx 'SHA256SUMS.sig' <<<"$assets" && [ "${RESIGN:-}" != 1 ] && [ "${IMAGES_ONLY:-}" != 1 ]; then
	echo "release-sign.sh: $tag is already signed (RESIGN=1 to replace the signature)" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "==> downloading $repo $tag"
gh release download "$tag" --repo "$repo" --dir "$work/ci" --pattern 'viiwork_*.tar.gz' --pattern SHA256SUMS
if [ "${IMAGES_ONLY:-}" = 1 ]; then
	gh release download "$tag" --repo "$repo" --dir "$work/ci" --pattern SHA256SUMS.sig
fi
(cd "$work/ci" && sha256sum --check --strict SHA256SUMS)

echo "==> rebuilding $tag from $commit"
mkdir -p "$work/src"
git -C "$src_repo" archive "$commit" | tar -x -C "$work/src"
(cd "$work/src" && ./scripts/build-release.sh "$tag" "$work/local" >/dev/null)
(cd "$work/src" && GOTOOLCHAIN="go$(sed -n 's/^go //p' go.mod)" go build -o "$work/viiwork-release" ./cmd/viiwork-release)

echo "==> comparing"
for archive in "$work/local"/viiwork_*.tar.gz; do
	name=$(basename "$archive")
	[ -f "$work/ci/$name" ] || { echo "release-sign.sh: CI did not publish $name" >&2; exit 1; }
	"$work/viiwork-release" compare "$work/ci/$name" "$work/local/${name%.tar.gz}"
done

# verify_sig checks SHA256SUMS.sig with the test key when given, else with
# COMMIT's compiled-in keys — which must accept it, or no node would.
verify_sig() {
	if [ -n "${RELEASE_SIGN_PUB:-}" ]; then
		"$work/viiwork-release" verify --pub "$RELEASE_SIGN_PUB" --version "$tag" "$work/ci/SHA256SUMS" "$work/ci/SHA256SUMS.sig"
	else
		"$work/viiwork-release" verify --version "$tag" "$work/ci/SHA256SUMS" "$work/ci/SHA256SUMS.sig"
	fi
}

if [ "${IMAGES_ONLY:-}" = 1 ]; then
	echo "==> verifying the existing signature"
	verify_sig
else
echo "==> signing"
# sign itself refuses a SHA256SUMS that lists anything but exactly this
# version's archives — every one of which was compared above.
"$work/viiwork-release" sign --key "$key" --version "$tag" "$work/ci/SHA256SUMS"
verify_sig
gh release upload "$tag" --repo "$repo" "$work/ci/SHA256SUMS.sig" --clobber
echo "==> $tag signed"
fi

if [ "${SKIP_IMAGES:-}" != 1 ]; then
	echo "==> publishing engine images"
	# The images carry the local build, which the compare just proved equal
	# to what was signed — never anything downloaded.
	registry=${IMAGE_REGISTRY:-ghcr.io/janit}
	# The login is written to a Docker config inside this run's temporary
	# directory, which the trap removes: the gh token (repo and workflow
	# scopes) must not outlive the run on the machine holding the key.
	export DOCKER_CONFIG="$work/docker"
	if [ "${registry%%/*}" = ghcr.io ] || [ "${IMAGE_LOGIN:-}" = 1 ]; then
		gh auth token | ${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.22.1} \
			auth login "${registry%%/*}" -u "$(gh api user --jq .login)" --password-stdin
	fi
	(cd "$work/src" && ./scripts/release-images.sh "$tag" "$work/local" "$registry")
fi
