#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=${VERSION:-0.1.0}
component=${1:-all}
case "$component" in all|dotnet|nodejs|python|go|provider) ;; *) echo "Unknown package component: $component" >&2; exit 1 ;; esac
case "${2:-}" in ''|--no-build) ;; *) echo "Unknown packaging option: $2" >&2; exit 1 ;; esac
build_sdk() {
  if [[ "${2:-}" != --no-build ]]; then
    bash scripts/build-sdk.sh "$1"
  fi
}
mkdir -p dist
if [[ "$component" == all || "$component" == dotnet ]]; then
  build_sdk dotnet "${2:-}"
  dotnet pack sdk/dotnet -c Release -p:Version="$version" --no-build -o "$PWD/dist"
fi
if [[ "$component" == all || "$component" == nodejs ]]; then
  build_sdk nodejs "${2:-}"
  (cd sdk/nodejs && cp package.json README.md bin/ && npm pack ./bin --pack-destination ../../dist)
fi
if [[ "$component" == all || "$component" == python ]]; then
  python3 -m build sdk/python --outdir dist
fi
if [[ "$component" == all || "$component" == go ]]; then
  build_sdk go "${2:-}"
  tar -czf "dist/pulumi-truenas-go-v${version}.tar.gz" -C sdk go go.mod go.sum
fi
# CI validates the native provider on every build; only releases need all platforms.
# Local packaging keeps producing the complete release by default.
if [[ "$component" == provider || ( "$component" == all && "${PACKAGE_PROVIDER_ARCHIVES:-true}" == true ) ]]; then
  # Optional filters let CI build exactly one target per runner.
  case "${PROVIDER_OS:-}" in ''|linux|darwin|windows) ;; *) echo "Unsupported provider OS: $PROVIDER_OS" >&2; exit 1 ;; esac
  case "${PROVIDER_ARCH:-}" in ''|amd64|arm64) ;; *) echo "Unsupported provider architecture: $PROVIDER_ARCH" >&2; exit 1 ;; esac
  for os in ${PROVIDER_OS:-linux darwin windows}; do
    for arch in ${PROVIDER_ARCH:-amd64 arm64}; do
      name=pulumi-resource-truenas
      if [ "$os" = windows ]; then name+=.exe; fi
      target="dist/${os}-${arch}"
      mkdir -p "$target"
      (cd provider && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X github.com/jetersen/pulumi-truenas/provider/pkg/version.Version=$version" \
        -o "../$target/$name" ./cmd/pulumi-resource-truenas)
      tar -czf "dist/pulumi-resource-truenas-v${version}-${os}-${arch}.tar.gz" -C "$target" "$name"
    done
  done
fi
if [[ "$component" == all ]]; then
  (cd dist && sha256sum ./*.nupkg ./*.tgz ./*.whl ./*.tar.gz > checksums.txt)
fi
