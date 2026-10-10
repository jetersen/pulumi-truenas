#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
component=${1:?Specify dotnet, nodejs, python, or go}
version=${VERSION:-0.1.0}
case "$component" in
  dotnet)
    dotnet build sdk/dotnet -c Release -p:Version="$version" -p:GeneratePackageOnBuild=false
    ;;
  nodejs)
    (cd sdk/nodejs && npm install --ignore-scripts --no-audit --no-fund && npm run build \
      && cp package.json bin/ && node -e 'if (!require("./bin").App) process.exit(1)')
    ;;
  python)
    test -f sdk/python/jetersen_pulumi_truenas/__init__.py
    python3 -m compileall -q sdk/python/jetersen_pulumi_truenas
    ;;
  go)
    (cd sdk && go test ./go/...)
    ;;
  *) echo "Unknown SDK component: $component" >&2; exit 1 ;;
esac
