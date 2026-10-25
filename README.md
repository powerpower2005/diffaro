# Diffaro

Read-only GitOps PR previews for GitHub and Argo CD. Diffaro renders the PR's **merge-base and head**, compares the final Kubernetes targets, and reports Application/resource changes and incomplete coverage. It is deterministic and AI-free; live cluster state is never mixed into the diff.

## Install

Clone this repository or extract its source archive, then run from its root. Use Go 1.26 or newer; `go.mod` selects Go 1.27.3. The first build needs network access to download the toolchain and dependencies.

```sh
go build -trimpath -o bin/diffaro ./cmd/diffaro
export PATH="$PWD/bin:$PATH"
diffaro --version
```

Supported native platforms are Linux amd64 and macOS amd64/arm64. Linux arm64 can be cross-built explicitly.

<details>
<summary>Install a release archive instead</summary>

When packages are available, download the archive and `SHA256SUMS` from this repository's Releases page. Verify the selected archive before extraction (replace the version):

```sh
grep '  diffaro_v0.2.0_linux_amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf diffaro_v0.2.0_linux_amd64.tar.gz
./diffaro --version
```

On macOS, use `shasum -a 256 -c -` for the selected checksum line. Put the executable on PATH or use its full path. Packages contain the executable, README and license notices; Go and Git are not required at runtime. Packages have no OS code signing or macOS notarization: checksums verify content, not publisher identity.

</details>

## Quick start

### 1. Try the demo

```sh
diffaro demo --json-output demo.json
```

No credentials or network are needed. Exit **1** means changes were found. The demo uses synthetic context and does not verify your Argo CD.

### 2. Create your profile

Prepare GitHub and Argo CD tokens with [read access](docs/configuration.md#access). The local Helm backend below needs no Kubernetes access. Your machine must reach the APIs and supported dependencies and trust their TLS certificates.

In Bash, enter tokens without putting their values in command history. CI can use its secret store:

```sh
read -r -s -p 'GitHub read token: ' DIFFARO_GITHUB_TOKEN
printf '\n'
read -r -s -p 'Argo CD read token: ' DIFFARO_ARGOCD_TOKEN
printf '\n'
export DIFFARO_GITHUB_TOKEN DIFFARO_ARGOCD_TOKEN
```

Replace the fictional endpoint and PR. Use `--single-instance` only when child Application CRs belong to the same Argo CD instance; otherwise configure [routing](docs/configuration.md#selection-and-routing).

**Init saves available tokens as plaintext in `diffaro.local.yaml`.** Run it in a private directory, keep the file out of Git and protect it with `0600` permissions. Existing files are never overwritten.

```sh
diffaro init --endpoint https://argocd.example.invalid \
  --pr https://github.com/example/charts/pull/123 --single-instance --local-helm
```

### 3. Check and compare

Run doctor first and resolve reported connection/render issues before running diff. Both commands explicitly use the profile you created:

```sh
diffaro doctor --config diffaro.local.yaml --render --pr 123
diffaro diff --config diffaro.local.yaml --pr 123 --json-output preview.json
```

Change the PR number to reuse the profile. Saved tokens override environment tokens; see [credential rotation](docs/configuration.md#authentication-precedence). Use `diffaro COMMAND --help` for options.

## Read the result

Check status, failed/skipped entries and graph completeness, then confirm revisions, instance and destination. **Exit 0/1 does not prove complete coverage.** Exit 0 means no observed changes; exit 1 means changes were found. Other codes are in the [reference](docs/reference.md#exit-codes).

`preview.json` holds evidence from the same comparison. Changed inputs help trace resource changes. Removed Applications show conditional pruning candidates, not guaranteed live deletion.

## Documentation

- [Configuration](docs/configuration.md): credentials, permissions, routing and render backends.
- [Reference](docs/reference.md): report fields, exit/error codes and exact comparison rules.
- [Security](SECURITY.md): vulnerability reporting and protecting credentials and artifacts.

## Scope

The v0.x line is for pilot adoption. Supported sources are Git Helm and external Git values, including multi-source, recursive App-of-Apps and multiple Argo CD instances. Target changes are evidence for reviewers, not measured outages or a deployment safety guarantee. Custom Argo CD environment equivalence remains unverified.

<details>
<summary>Selection boundaries and unsupported features</summary>

ApplicationSet support selects currently readable generated Applications, not base/head generator output. Changed-path selection skips only proven-unaffected inputs. Impact rules, Kustomize, plain directories, CMP, Helm/OCI chart repository sources, sourceHydrator, fork PRs, global indirect consumer discovery, Argo CD ignore-rule application, a web UI and sync/rollback are outside scope.

</details>

## Troubleshooting

Follow each issue's `action`, then rerun. `--verbose` adds safe stage/instance details. Look up [issue/reason codes](docs/reference.md#issue-codes) as needed.

<details>
<summary>Common problems</summary>

| Symptom | Check |
|---|---|
| Authentication or read denied | Token, read permission, Application namespace and instance. Never add sync/update/delete access. |
| Connection or TLS failure | API endpoint, DNS/VPN, network route and trusted CA; login-page redirects are not API access. |
| Child skipped or targets unknown | Missing context and [backend choice](docs/configuration.md#render-backends). Skipped targets are not unchanged. |
| Partial failure | Repair failed branches; independently completed output remains available. |
| Chart render failure | Reported template location and reason; reproduce with the recorded inputs. |

For offline examples of partial failure or incomplete coverage, use `diffaro demo --scenario partial-failure` (exit 3) or `--scenario skipped-context`.

</details>

### Report a problem

<details>
<summary>Create a reviewed public debug bundle</summary>

For `internal_error` or a reproducible wrong result, attach the version, exit code and a reviewed **public** bundle to a repository issue:

```sh
diffaro diff --config diffaro.local.yaml --pr 123 --debug-bundle diffaro-bundle.tar.gz --debug-bundle-level public
tar -xzOf diffaro-bundle.tar.gz diffaro-debug-bundle/README.md
tar -xzOf diffaro-bundle.tar.gz diffaro-debug-bundle/bundle.json
```

Set the level explicitly because profile modes can select `private`. Public bundles pseudonymize names and exclude values/raw errors. Inspect before sharing and keep the `.names.json` map private. Never attach private bundles, credential profiles or rendered secrets to public issues. Report vulnerabilities through [SECURITY.md](SECURITY.md).

</details>

## CI use

<details>
<summary>CI commands and artifact safety</summary>

Use a reviewed binary, trusted profile and read-only secret-store credentials. Never execute PR-supplied profiles or helpers with trusted credentials.

```sh
set +e
diffaro diff --config profile.yaml --pr "$PR_URL" --format pr-markdown --output preview.md --json-output preview.json
status=$?
set -e
case "$status" in 0|1) ;; *) exit "$status" ;; esac
```

Apply the [result checks](#read-the-result) and compare the report's head SHA with the current PR head before publishing; cancel superseded runs. Integrators own publication and retention. Follow [credential rotation](docs/configuration.md#authentication-precedence) for environment secrets. Init's [CI mode](docs/configuration.md#run-defaults-and-modes) saves reports and diagnostics, including a private debug bundle; review it before sharing artifacts.

</details>

## Development

<details>
<summary>Run the product checks</summary>

```sh
bash scripts/check.sh
```

Linux/macOS checks require Bash, a C compiler for race detection and recent Git with `GIT_NO_LAZY_FETCH` support. Keep temporary test directories outside a Git checkout. Tests use deterministic fake backends and cover read-only behavior, revisions, partial failures, bounds, redaction and the installed executable; no Python or production credentials are needed. Workflows run checks and tagged binary releases from the repository root.

</details>

Licensed under [Apache-2.0](LICENSE).
