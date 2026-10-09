#!/usr/bin/env bash
# Build every release target of VERSION into OUTDIR:
#
#	OUTDIR/viiwork_VERSION_OS_ARCH/{viiwork,viiwork-accept,LICENSE}
#	OUTDIR/viiwork_VERSION_OS_ARCH.tar.gz
#	OUTDIR/SHA256SUMS
#
# Used by the release workflow and by release-sign.sh's local rebuild. The
# archives are deterministic (sorted, fixed mtime and owner, gzip without a
# timestamp) so the whole directory is reproducible, not only the binaries.
set -euo pipefail
cd "$(dirname "$0")/.."
if [ $# -ne 2 ]; then
	echo "usage: build-release.sh VERSION OUTDIR" >&2
	exit 2
fi
version=$1
out=$2
if ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; then
	echo "build-release.sh: not a release version: $version" >&2
	exit 2
fi
# The compiler is part of the bytes. Pin it to go.mod's exact version, so a
# publisher whose local Go is newer still rebuilds what CI built.
GOTOOLCHAIN="go$(sed -n 's/^go //p' go.mod)"
export GOTOOLCHAIN
# Nothing from the caller's Go environment may reach the bytes: build flags,
# experiments and the FIPS mode are cleared, and the CPU levels are the
# baselines every supported machine runs.
unset GOFLAGS GOEXPERIMENT GOFIPS140
export GOAMD64=v1 GOARM64=v8.0
mkdir -p "$out"
for target in linux/amd64 linux/arm64 darwin/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name="viiwork_${version}_${os}_${arch}"
	rm -rf "${out:?}/$name" "$out/$name.tar.gz"
	mkdir -p "$out/$name"
	GOOS=$os GOARCH=$arch ./scripts/gobuild.sh ./cmd/viiwork "$out/$name/viiwork" "$version"
	GOOS=$os GOARCH=$arch ./scripts/gobuild.sh ./cmd/viiwork-accept "$out/$name/viiwork-accept" "$version"
	cp LICENSE "$out/$name/LICENSE"
	tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$out" -cf - "$name" |
		gzip -n -9 >"$out/$name.tar.gz"
done
(cd "$out" && sha256sum "viiwork_${version}"_*.tar.gz >SHA256SUMS)
echo "built $version into $out"
