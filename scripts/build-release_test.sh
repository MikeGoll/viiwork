#!/usr/bin/env bash
# Two release builds of the committed tree, from two different directories,
# must be byte-identical — archives and SHA256SUMS included. release-sign.sh
# relies on this to check CI's binaries against a local rebuild.
#
#	bash scripts/build-release_test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for d in a b; do
	mkdir -p "$tmp/$d/src"
	git archive HEAD | tar -x -C "$tmp/$d/src"
done
(cd "$tmp/a/src" && ./scripts/build-release.sh v0.0.0-repro "$tmp/a/out" >/dev/null)
# The second build runs in a hostile Go environment: none of it may reach the
# bytes, or the publisher's shell would decide whether a release can be signed.
(cd "$tmp/b/src" && GOFLAGS=-buildmode=pie GOAMD64=v3 GOARM64=v8.2 GOEXPERIMENT=loopvar \
	./scripts/build-release.sh v0.0.0-repro "$tmp/b/out" >/dev/null)
fail=0
count=0
for f in $(cd "$tmp/a/out" && find . -type f | sort); do
	count=$((count + 1))
	if cmp -s "$tmp/a/out/$f" "$tmp/b/out/$f"; then
		printf 'ok    %s\n' "$f"
	else
		printf 'FAIL  %s differs\n' "$f"
		fail=1
	fi
done
# 3 targets x (3 files + 1 archive) + SHA256SUMS
if [ "$count" -ne 13 ]; then
	printf 'FAIL  %d files, want 13\n' "$count"
	fail=1
fi
exit $fail
