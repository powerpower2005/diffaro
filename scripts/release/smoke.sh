#!/usr/bin/env bash
# Native Linux/macOS installation contract: checksums, minimal files and no tools at runtime.
set -euo pipefail
version=${1:?release version required}
revision=${2:?full source revision required}
artifacts=${3:-dist}
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ && "$revision" =~ ^[0-9a-f]{40,64}$ ]] || { echo 'Invalid version/revision' >&2; exit 1; }
case "$(uname -s)" in Linux) platform=linux ;; Darwin) platform=darwin ;; *) echo 'Unsupported OS' >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64) architecture=amd64 ;; arm64|aarch64) architecture=arm64 ;; *) echo 'Unsupported architecture' >&2; exit 1 ;; esac
artifacts=$(cd "$artifacts" && pwd -P)
name="diffaro_${version}_${platform}_${architecture}.tar.gz"
checksum=$(awk -v name="$name" '$2 == name { print $1 }' "$artifacts/SHA256SUMS")
[[ "$checksum" =~ ^[0-9a-f]{64}$ ]] || { echo 'Expected one native checksum' >&2; exit 1; }
if command -v sha256sum >/dev/null; then
  actual=$(sha256sum "$artifacts/$name"); actual=${actual%% *}
else
  actual=$(shasum -a 256 "$artifacts/$name"); actual=${actual%% *}
fi
[[ "$actual" == "$checksum" ]] || { echo 'Checksum mismatch' >&2; exit 1; }
members=$(tar -tzf "$artifacts/$name")
while IFS= read -r member; do
  case "$member" in diffaro|LICENSE|README.txt|licenses/*) ;; *) echo 'Archive contains files outside the minimal distribution' >&2; exit 1 ;; esac
  case "/$member/" in */../*|*/./*|*//*) echo 'Unsafe archive path' >&2; exit 1 ;; esac
done <<< "$members"
tar -tvzf "$artifacts/$name" | while IFS= read -r entry; do
  [[ "$entry" == -* ]] || { echo 'Archive members must be regular files' >&2; exit 1; }
done
temporary=$(cd "${TMPDIR:-/tmp}" && pwd -P)
installation=$(mktemp -d "$temporary/diffaro-install.XXXXXXXXXX")
cleanup() {
  case "$installation" in "$temporary"/diffaro-install.*) rm -rf -- "$installation" ;; *) echo 'Refusing unsafe cleanup' >&2; return 1 ;; esac
}
trap cleanup EXIT
tar -xzf "$artifacts/$name" -C "$installation"
for required in diffaro LICENSE README.txt licenses/go-LICENSE.txt; do
  [[ -f "$installation/$required" && -s "$installation/$required" ]] || { echo 'Missing executable/document/license' >&2; exit 1; }
done
[[ -x "$installation/diffaro" ]] || { echo 'Binary lost executable permissions' >&2; exit 1; }
output=$(PATH='' "$installation/diffaro" --version)
[[ "$output" == "Diffaro $version; revision ${revision:0:12};"* && "$output" != *modified* ]] || { echo 'Packaged build provenance mismatch' >&2; exit 1; }
for command in init doctor diff; do
  help=$(PATH='' "$installation/diffaro" "$command" --help)
  [[ "$help" == *"Usage: diffaro $command"* ]] || { echo 'Packaged command help failed' >&2; exit 1; }
done
cat > "$installation/profile.yaml" <<'YAML'
version: v1alpha1
git:
  apiURL: https://api.github.com
  repository: example/charts
  repositoryURL: https://github.com/example/charts.git
  tokenEnv: DIFFARO_GITHUB_TOKEN
instances:
  - id: default
    endpoint: https://argocd.example.invalid
    tokenEnv: DIFFARO_ARGOCD_TOKEN
applications:
  - instance: default
    namespace: argocd
    name: api
YAML
PATH='' "$installation/diffaro" validate --config "$installation/profile.yaml"
echo "Release install smoke passed: $platform/$architecture; $output; PATH empty."
