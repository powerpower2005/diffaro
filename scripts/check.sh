#!/usr/bin/env bash
# Product checks without optional integration/diagnostic helper programs.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -n "$(gofmt -l cmd internal scripts/release)" ]]; then
  echo 'Go files need formatting' >&2
  exit 1
fi
go vet ./...
go test -race -count=1 ./...
fuzztime="${DIFFARO_FUZZTIME:-10s}"
if [[ "$fuzztime" != "0" ]]; then
  for target in FuzzParseManifests FuzzParseValues FuzzLineHunks; do
    go test ./internal/manifest -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime"
  done
  go test ./internal/yamlview -run '^$' -fuzz '^FuzzLines$' -fuzztime "$fuzztime"
fi
mkdir -p bin
go build -trimpath -o bin/diffaro ./cmd/diffaro
DIFFARO_TEST_INSTALLED_BINARY="$PWD/bin/diffaro" go test -count=1 ./internal/cli -run '^Test(InstalledBinary|BundledHelmFirstUseAndNewChild|CatalogInitToDoctorAndMultiRepositoryPRArtifacts)'
echo 'Product checks passed (unit/mock integrations, race and installed binary).'
