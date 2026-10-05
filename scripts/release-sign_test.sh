#!/usr/bin/env bash
# Tests release-sign.sh end to end against a stand-in `gh` that serves a
# release's assets from a local directory, with a throwaway key:
#
#	bash scripts/release-sign_test.sh
#
# It builds the committed tree several times, so it takes a few minutes.
set -euo pipefail
cd "$(dirname "$0")/.."
root=$(pwd)
tmp=$(mktemp -d)
# chmod first: a Go module cache, should one land here, is read-only.
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT
version=v0.0.0-signtest
# Case 6 changes HOME; Go keeps its real caches rather than filling a new one.
export GOPATH=${GOPATH:-$(go env GOPATH)} GOMODCACHE=${GOMODCACHE:-$(go env GOMODCACHE)} GOCACHE=${GOCACHE:-$(go env GOCACHE)}
fail=0

# A stand-in gh: `release view` lists $FAKE_ASSETS, `release download` copies
# from it, `release upload` copies into $FAKE_UPLOADS.
mkdir -p "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -eu
case "$1 ${2:-}" in
"auth token") echo gho_FAKE_TOKEN_DO_NOT_KEEP; exit 0 ;;
"api user") echo tester; exit 0 ;;
esac
[ "$1" = release ] || exit 1
cmd=$2
shift 2
case $cmd in
view) ls "$FAKE_ASSETS" ;;
download)
	dir=
	while [ $# -gt 0 ]; do
		case $1 in --dir) dir=$2; shift 2 ;; *) shift ;; esac
	done
	mkdir -p "$dir"
	cp "$FAKE_ASSETS"/viiwork_*.tar.gz "$FAKE_ASSETS"/SHA256SUMS "$dir"/
	if [ -f "$FAKE_ASSETS/SHA256SUMS.sig" ]; then cp "$FAKE_ASSETS/SHA256SUMS.sig" "$dir"/; fi
	;;
upload)
	mkdir -p "$FAKE_UPLOADS"
	for a in "$@"; do
		if [ -f "$a" ]; then cp "$a" "$FAKE_UPLOADS"/; fi
	done
	;;
esac
EOF
chmod +x "$tmp/bin/gh"

# CI's release, built from the committed tree.
git archive HEAD | (mkdir -p "$tmp/src" && tar -x -C "$tmp/src")
(cd "$tmp/src" && ./scripts/build-release.sh "$version" "$tmp/ci" >/dev/null)
(cd "$tmp/src" && go build -o "$tmp/viiwork-release" ./cmd/viiwork-release)
"$tmp/viiwork-release" keygen --key "$tmp/key" --pub "$tmp/test.pub" >/dev/null

# run_case NAME EXPECT_SIGNED: stage $tmp/assets, run release-sign.sh, and
# check whether a signature was uploaded.
run_case() {
	rm -rf "$tmp/uploads"
	set +e
	PATH="$tmp/bin:$PATH" FAKE_ASSETS="$tmp/assets" FAKE_UPLOADS="$tmp/uploads" \
		VIIWORK_RELEASE_KEY="$tmp/key" RELEASE_SIGN_PUB="$tmp/test.pub" SKIP_IMAGES="${SKIP_IMAGES-1}" \
		./scripts/release-sign.sh "$version" >"$tmp/out" 2>&1
	code=$?
	set -e
	if [ "$2" = yes ] && [ "$code" -eq 0 ] && [ -f "$tmp/uploads/SHA256SUMS.sig" ]; then
		printf 'ok    %s\n' "$1"
	elif [ "$2" = no ] && [ "$code" -ne 0 ] && [ ! -e "$tmp/uploads/SHA256SUMS.sig" ]; then
		printf 'ok    %s (refused: %s)\n' "$1" "$(tail -1 "$tmp/out")"
	else
		printf 'FAIL  %s: exit %d, signed=%s\n' "$1" "$code" "$([ -f "$tmp/uploads/SHA256SUMS.sig" ] && echo yes || echo no)"
		sed 's/^/      /' "$tmp/out"
		fail=1
	fi
}

stage() { rm -rf "$tmp/assets" && mkdir -p "$tmp/assets" && cp "$tmp/ci"/viiwork_*.tar.gz "$tmp/ci/SHA256SUMS" "$tmp/assets"/; }

# 1. CI built exactly the committed tree: signed, and the signature verifies.
stage
run_case "genuine release is signed" yes
if [ -f "$tmp/uploads/SHA256SUMS.sig" ] &&
	! "$tmp/viiwork-release" verify --pub "$tmp/test.pub" --version "$version" "$tmp/assets/SHA256SUMS" "$tmp/uploads/SHA256SUMS.sig" >/dev/null; then
	echo "FAIL  the uploaded signature does not verify"
	fail=1
fi

# 2. One byte of one binary differs: nothing is signed.
stage
name="viiwork_${version}_linux_amd64"
mkdir -p "$tmp/evil" && tar -xzf "$tmp/assets/$name.tar.gz" -C "$tmp/evil"
printf 'X' | dd of="$tmp/evil/$name/viiwork" bs=1 seek=1000 conv=notrunc 2>/dev/null
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$tmp/evil" -cf - "$name" | gzip -n >"$tmp/assets/$name.tar.gz"
(cd "$tmp/assets" && sha256sum viiwork_*.tar.gz >SHA256SUMS)
run_case "a flipped byte is refused" no

# 3. SHA256SUMS smuggles in another version's archive: nothing is signed.
stage
cp "$tmp/assets/$name.tar.gz" "$tmp/assets/viiwork_v9.9.9_linux_amd64.tar.gz"
(cd "$tmp/assets" && sha256sum "viiwork_${version}_linux_amd64.tar.gz" "viiwork_${version}_linux_arm64.tar.gz" viiwork_v9.9.9_linux_amd64.tar.gz >SHA256SUMS)
run_case "a smuggled archive is refused" no

# 4. Already signed: refused without RESIGN=1.
stage
touch "$tmp/assets/SHA256SUMS.sig"
run_case "an already signed release is refused" no

# 5. The engine images are published from the build that was compared, not
# from anything downloaded. A local registry stands in for ghcr.io.
reg=127.0.0.1:5057
if docker run -d --rm --name viiwork-signtest-registry -p "$reg:5000" registry:2 >/dev/null 2>&1; then
	stage
	SKIP_IMAGES= IMAGE_REGISTRY="$reg" FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure \
		run_case "a signed release publishes its images" yes
	img="$reg/viiwork-llamacpp-vulkan:$version"
	if docker pull -q "$img" >/dev/null 2>&1; then
		got=$(docker create "$img" | xargs -I{} sh -c 'docker cp {}:/usr/local/bin/viiwork - | tar -xOf - | sha256sum | cut -d" " -f1; docker rm {} >/dev/null')
		want=$(tar -xzOf "$tmp/assets/viiwork_${version}_linux_amd64.tar.gz" "viiwork_${version}_linux_amd64/viiwork" | sha256sum | cut -d' ' -f1)
		[ "$got" = "$want" ] && echo "ok    the image carries the signed binary" || { echo "FAIL  image binary $got, signed $want"; fail=1; }
		docker rmi -f "$img" >/dev/null 2>&1
	else
		echo "FAIL  no image was published"
		fail=1
	fi

	# 6. A registry login leaves no credentials behind: the token is written to
	# a Docker config inside the run's temporary directory, never $HOME.
	stage
	mkdir -p "$tmp/home"
	HOME="$tmp/home" SKIP_IMAGES= IMAGE_REGISTRY="$reg" IMAGE_LOGIN=1 REPUBLISH=1 FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure \
		run_case "a publish with a registry login" yes
	if ! grep -q "logged in via" "$tmp/out"; then
		echo "FAIL  no registry login happened: $(tail -1 "$tmp/out")"
		fail=1
	elif [ -e "$tmp/home/.docker" ]; then  # the token is stored base64-encoded: look for the config itself
		echo "FAIL  the registry token was left in \$HOME"
		fail=1
	else
		echo "ok    no registry token left behind"
	fi

	# 7. Images alone, for a release that is already signed (the recovery
	# when a push failed after signing): the existing signature is verified,
	# nothing is signed or uploaded again.
	stage
	"$tmp/viiwork-release" sign --key "$tmp/key" --version "$version" --out "$tmp/assets/SHA256SUMS.sig" "$tmp/assets/SHA256SUMS" >/dev/null
	rm -rf "$tmp/uploads"
	if PATH="$tmp/bin:$PATH" FAKE_ASSETS="$tmp/assets" FAKE_UPLOADS="$tmp/uploads" VIIWORK_RELEASE_KEY="$tmp/key" \
		RELEASE_SIGN_PUB="$tmp/test.pub" IMAGES_ONLY=1 IMAGE_REGISTRY="$reg" REPUBLISH=1 FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure \
		./scripts/release-sign.sh "$version" >"$tmp/out" 2>&1 && [ ! -e "$tmp/uploads/SHA256SUMS.sig" ] && grep -q "pushed $reg/viiwork-llamacpp-vulkan" "$tmp/out"; then
		echo "ok    images only, for a signed release"
	else
		echo "FAIL  images only: $(tail -2 "$tmp/out")"
		fail=1
	fi
	# ...and refused for a release whose signature does not verify.
	stage
	echo "bm90IGEgc2lnbmF0dXJl" >"$tmp/assets/SHA256SUMS.sig"
	if PATH="$tmp/bin:$PATH" FAKE_ASSETS="$tmp/assets" FAKE_UPLOADS="$tmp/uploads" VIIWORK_RELEASE_KEY="$tmp/key" \
		RELEASE_SIGN_PUB="$tmp/test.pub" IMAGES_ONLY=1 IMAGE_REGISTRY="$reg" REPUBLISH=1 FLAVOURS=llamacpp-vulkan CRANE_FLAGS=--insecure \
		./scripts/release-sign.sh "$version" >"$tmp/out" 2>&1; then
		echo "FAIL  images only published for an unverified release"
		fail=1
	else
		echo "ok    images only refuses an unverified release"
	fi
	docker rm -f viiwork-signtest-registry >/dev/null 2>&1
else
	echo "skip  images: no docker"
fi

exit $fail
