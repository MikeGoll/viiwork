#!/usr/bin/env sh
# Build one viiwork binary. The only place build flags live: make, the release
# workflow and release-sign.sh's local rebuild all come through here, so a
# release built in CI and one rebuilt on the publisher's machine cannot differ
# by a flag.
#
#	scripts/gobuild.sh PACKAGE OUTPUT VERSION     (GOOS/GOARCH from the environment)
#
# Static (no cgo), and free of the build path and VCS stamp, so the same tree
# at the same version builds to the same bytes anywhere.
set -eu
if [ $# -ne 3 ]; then
	echo "usage: gobuild.sh PACKAGE OUTPUT VERSION" >&2
	exit 2
fi
# The llama.cpp pin and its macOS digest ride along in every binary
# (viiwork --build-info), so a downloaded viiwork knows which llama.cpp a Mac
# should fetch and which bytes it must be.
pins="$(dirname "$0")/../docker/pins.env"
LLAMA_CPP_VERSION=
LLAMA_CPP_MACOS_SHA256=
[ -f "$pins" ] && . "$pins"
CGO_ENABLED=0 exec go build -trimpath -buildvcs=false \
	-ldflags "-X main.version=$3 -X main.llamaCppPin=$LLAMA_CPP_VERSION -X main.llamaCppMacSHA256=$LLAMA_CPP_MACOS_SHA256" -o "$2" "$1"
