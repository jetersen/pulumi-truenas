#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=${VERSION:-0.1.0}
component=${1:-all}
case "$component" in all|dotnet|nodejs|python|go|provider) ;; *) echo "Unknown package component: $component" >&2; exit 1 ;; esac
mkdir -p dist
if [[ "$component" == all || "$component" == dotnet ]]; then
  dotnet build sdk/dotnet -c Release -p:Version="$version"
  dotnet pack sdk/dotnet -c Release -p:Version="$version" --no-build -o "$PWD/dist"
fi
if [[ "$component" == all || "$component" == nodejs ]]; then
  (cd sdk/nodejs && npm install --ignore-scripts --no-audit --no-fund && npm run build \
    && cp package.json README.md bin/ && node -e 'if (!require("./bin").App) process.exit(1)' \
    && npm pack ./bin --pack-destination ../../dist)
fi
if [[ "$component" == all || "$component" == python ]]; then
  python3 -m build sdk/python --outdir dist
fi
if [[ "$component" == all || "$component" == go ]]; then
  (cd sdk && go test ./go/...)
  tar -czf "dist/pulumi-truenas-go-v${version}.tar.gz" -C sdk go go.mod go.sum
fi
# CI validates the native provider on every build; only releases need all platforms.
# Local packaging keeps producing the complete release by default.
if [[ "$component" == provider || ( "$component" == all && "${PACKAGE_PROVIDER_ARCHIVES:-true}" == true ) ]]; then
  for os in linux darwin windows; do
    for arch in amd64 arm64; do
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
