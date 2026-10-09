#!/usr/bin/env sh
# The engine pins live in docker/pins.env; the Dockerfiles' ARG defaults and
# the stamps in every binary must agree with it.
#
#	sh scripts/pins_test.sh
set -eu
cd "$(dirname "$0")/.."
. docker/pins.env
fail=0
check() {
	if [ "$2" = "$3" ]; then
		printf 'ok    %s = %s\n' "$1" "$2"
	else
		printf 'FAIL  %s = %s, pins.env says %s\n' "$1" "$2" "$3"
		fail=1
	fi
}
check "Dockerfile.rocm LLAMA_CPP_VERSION" "$(sed -n 's/^ARG LLAMA_CPP_VERSION=//p' docker/Dockerfile.rocm)" "$LLAMA_CPP_VERSION"
check "Dockerfile.vllm VLLM_VERSION" "$(sed -n 's/^ARG VLLM_VERSION=//p' docker/Dockerfile.vllm)" "$VLLM_VERSION"
check "Dockerfile.strata STRATA_VERSION" "$(sed -n 's/^ARG STRATA_VERSION=//p' docker/Dockerfile.strata)" "$STRATA_VERSION"
check "Dockerfile.strata STRATA_COMMIT" "$(sed -n 's/^ARG STRATA_COMMIT=//p' docker/Dockerfile.strata)" "$STRATA_COMMIT"
check "Dockerfile.strata STRATA_ROCM_BASE" "$(sed -n 's/^ARG STRATA_ROCM_BASE=//p' docker/Dockerfile.strata)" "$STRATA_ROCM_BASE"
check "Dockerfile.strata-cuda STRATA_VERSION" "$(sed -n 's/^ARG STRATA_VERSION=//p' docker/Dockerfile.strata-cuda)" "$STRATA_VERSION"
check "gfx906 patch for STRATA_VERSION exists" "$([ -f "docker/strata/gfx906-$STRATA_VERSION.patch" ] && echo yes || echo no)" yes
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
./scripts/gobuild.sh ./cmd/viiwork "$tmp/viiwork" v0.0.0-pins
check "viiwork --build-info llama_cpp" "$("$tmp/viiwork" --build-info | sed -n 's/.*"llama_cpp":"\([^"]*\)".*/\1/p')" "$LLAMA_CPP_VERSION"
check "viiwork --build-info llama_cpp_macos_sha256" "$("$tmp/viiwork" --build-info | sed -n 's/.*"llama_cpp_macos_sha256":"\([^"]*\)".*/\1/p')" "$LLAMA_CPP_MACOS_SHA256"
case "$LLAMA_CPP_MACOS_SHA256" in
*[!0-9a-f]*) check "LLAMA_CPP_MACOS_SHA256 is lowercase hex" "no" "yes" ;;
esac
check "LLAMA_CPP_MACOS_SHA256 length" "${#LLAMA_CPP_MACOS_SHA256}" 64
exit $fail
