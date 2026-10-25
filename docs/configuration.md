# Configuration

Start with the [README quick start](../README.md#quick-start), then use this page to adapt the profile to your environment. Profiles use `v1alpha1`. `diffaro validate --config FILE` checks them offline; `doctor --render --pr NUMBER` checks actual targets. Profile paths, routing, policies and helper executables are trusted operator settings, never PR-controlled inputs. Report fields and comparison behavior are in the [reference](reference.md).

Read [authentication](#authentication-precedence), [access](#access), [selection and routing](#selection-and-routing), [render backends](#render-backends), [multi-source](#multi-source-and-external-git-values) or [limits](#redaction-and-limits).

## Authentication precedence

GitHub and each Argo CD instance choose credentials independently:

1. Explicit `--git-token`, `--argocd-token` or `--instance-token ID=TOKEN`.
2. `git.token` or `instances[].token` in the profile.
3. The variable named by `tokenEnv`, if no token is configured.

Invalid/empty flags are rejected without fallback. `--argocd-token` needs one instance; multiple instances use distinct `--instance-token` IDs. Overrides affect that run only. Init saves effective tokens, including environment tokens, to a new plaintext profile and never overwrites files/symlinks. Protect it with 0600 permissions and a private directory; tokens passed as arguments can appear in history/process listings. Edit the private profile to rotate a saved token.

Without `--config`, commands read `diffaro.yaml`, then `diffaro.local.yaml`. Init writes the latter by default. For CI secret-store rotation, remove saved `token` fields and retain `tokenEnv` entries.

## Access

| Connection | Required reads/access |
|---|---|
| GitHub | Contents, metadata and pull requests for the PR, charts and external values repositories |
| Argo CD API | `applications/get` and `projects/get` for selected roots/children/projects |
| Local Helm | The Argo CD reads above, settings API and `clusters/get` for destination capabilities |
| Repo-server | The Argo CD reads above; `clusters/get` for named/external destination capabilities or project destination checks; settings API if automatic context has no configured tracking method |
| Optional automatic context | Kubernetes reads for Argo CD ConfigMaps, repository/repo-creds Secrets and API discovery |
| Optional managed tunnel | Selected resource reads and `pods/portforward` create |

Local Helm needs no kubeconfig, kubectl, grpcurl or repository Secret access. Production sync/update/delete is never needed. Credentials and context are separate per Argo CD instance; a root token does not grant access to regional APIs. Public GitHub repositories allow anonymous reads, but use a read-only token to avoid the low shared unauthenticated quota. Endpoints must use trusted TLS; there is no TLS-verification bypass.

An Argo CD administrator can merge these fragments into existing configuration ([accounts](https://argo-cd.readthedocs.io/en/stable/operator-manual/user-management/), [RBAC](https://argo-cd.readthedocs.io/en/stable/operator-manual/rbac/)):

```yaml
# argocd-cm data
data:
  accounts.diffaro: apiKey
```

```csv
# argocd-rbac-cm data.policy.csv
p, role:diffaro, applications, get, demo/*, allow
p, role:diffaro, projects, get, demo, allow
p, role:diffaro, clusters, get, *, allow
g, diffaro, role:diffaro
```

The clusters line is for local Helm capabilities. With Applications in any namespace, use the corresponding project/namespace/name object, such as `demo/argocd/*`. Cover each selected namespace/project and check inherited default roles. Issue a read token, for example `argocd account generate-token --account diffaro --expires-in 24h`. Version API reads record compatibility without another RBAC line; matrix membership does not certify your environment.

## Run defaults and modes

Init writes recursive terminal defaults and a `ci` mode. Defaults apply to diff/doctor, a selected mode overrides defaults, and explicit flags win:

```yaml
run:
  defaults:
    recursive: true
    format: terminal
  modes:
    ci:
      format: pr-markdown
      output: preview.md
      jsonOutput: preview.json
      trace: trace.json
      debugBundle: diffaro-bundle-private.tar.gz
      debugBundleLevel: private
      quiet: true
```

Run `diffaro diff --mode ci --pr 123`; `diffaro validate` prints the effective defaults/modes. Settable options are `format`, `output`, `jsonOutput`, `trace`, `debugBundle`, `debugBundleLevel`, `recursive`, `quiet` and `verbose`. Relative outputs use the working directory. Doctor ignores diff-only outputs and uses recursion only with `--render`. Unknown mode names exit 4; at most 16 lowercase/digit/hyphen mode names are allowed.

## Selection and routing

`init --pr URL` creates repository discovery. Equivalent minimal profile:

```yaml
version: v1alpha1
git:
  apiURL: https://api.github.com
  repository: example/charts
  repositoryURL: https://github.com/example/charts.git
  tokenEnv: DIFFARO_GITHUB_TOKEN
instances:
  - id: test
    endpoint: https://argocd.example.invalid
    tokenEnv: DIFFARO_ARGOCD_TOKEN
    localHelm: true
discovery:
  mode: repository
  selection: changedPaths
routing:
  defaultInstance: test
```

Discovery selects readable Applications using the PR repository and recursively follows rendered children. `discovery.projects` optionally restricts projects. It cannot coexist with explicit `applications` or `applicationSets`; the PR URL must match `git.repositoryURL`. Init selects `changedPaths`; a hand-written profile without it uses `all`, and `init --selection all` writes that explicitly. Only proven-unaffected inputs may be skipped; uncertain candidates render. Explicit base/head runs render every candidate. Exact selection evidence is in the [reference](reference.md#repository-discovery).

For explicit roots, `init --application argocd/platform-root`; `--recursive` follows children. `init --application-set argocd/services` selects only current readable generated Applications by ownerReference, not a predicted base/head generated set. For multiple instances, `init --connections CATALOG --pr URL --local-children --local-helm` creates a profile from a trusted catalog with an `instances` list and optional `routing` settings. Catalog init is offline; doctor checks it.

For multiple instances, configure trusted `routing.parents`, `routing.clusters`, `routing.repositories` or `routing.defaultInstance` rules. The [routing reference](reference.md#recursive-graph-and-routing) defines their precedence and conflict handling; a child's workload destination does not identify its managing Argo CD instance.

### Repository URLs and local caches

Standard HTTPS and git-user SSH forms match the same host/path; trailing `.git`/slash is ignored. Other aliases, ports and usernames are not guessed. GitHub Enterprise init defaults to `https://HOST/api/v3`; use `--git-api` if different.

For read-through mirrors, use up to 8 HTTPS same-owner/repository `git.repositoryAliases` (`init --repository-alias URL`) or up to 8 pathless HTTPS `git.mirrorHosts` (`--mirror-host URL`). They widen source matching without changing the trusted Git API/PR identity; credentials are never sent to the mirror. Init reports mirror-only candidates and refuses an empty selection without the needed mapping.

```yaml
git:
  repositoryURL: https://github.example.invalid/example/charts
  mirrorHosts: [https://mirror.example.invalid]
  localRepositories:
    - name: example/charts
      path: /cache/git/example-charts.git
  gitExecutable: /usr/bin/git
```

Local repositories supply immutable content through read-only Git commands. Keep full clones fetched, including PR heads, before running Diffaro; the binary never fetches or runs hooks. Missing/partial-clone content falls back to the API. PR metadata and branch/tag resolution still use the API; size bounds still apply.

## Render backends

| Backend | Select with | New/reconfigured child targets |
|---|---|---|
| Argo CD Application API | Default | Skipped when the stored spec cannot represent them |
| Bundled local Helm | `--local-helm` or instance `localHelm: true` | Supported Git Helm specs rendered from immutable GitHub sources |
| External local Helm | `--helm-binary PATH` or instance `helmBinary` | Same bounded inputs with a trusted Helm 3/4 executable |
| Repo-server | Instance `repoServer` | Desired specs rendered with the reviewed server context |

Inputs preserve ordered `valueFiles`, inline `values`/`valuesObject`, parameters/forceString, file parameters, release name, destination and API capabilities. Choose one local/repo-server backend per instance. API-only analysis retains observed Application/edge changes but skips resources and descendants it cannot render. A skip never means no changes.

### Local Helm

Bundled upstream Helm v4.2.1 obtains authenticated Argo CD settings and destination capabilities. Dependencies must be vendored, same-repository file paths or supported public HTTPS downloads with locked versions. Additional private registry authentication is not supported. Public downloads reject non-public destination addresses and report safe host/cause details on failure.

Supported Helm fields are `valueFiles`, `values`, `valuesObject`, `parameters`, `fileParameters`, `releaseName`, `namespace`, `version`, `kubeVersion`, `apiVersions`, `ignoreMissingValueFiles`, `skipCrds`, `skipSchemaValidation`, `skipTests` and `passCredentials`; the last has no effect because no repository credentials are passed. Other fields, relevant `.argocd-source*.yaml` overrides, unsupported remote/glob values, overlapping parameters, missing capabilities or unsupported context remain explicit skips. A parameter/fileParameter name with an unescaped `=` or `,` fails rather than introducing another Helm assignment. Internal symlink targets are not followed; escaping archive paths fail.

CLI executable paths are relative to the working directory; profile/catalog paths are relative to that file. The native executable must report Helm 3/4 via `version --short`. Base/head use one fingerprinted snapshot; version and SHA-256 are reported. There is no fallback, shell wrapper, plugin or post-renderer execution. This is trusted native code, not an OS sandbox: never use a PR-supplied executable. Parameter values may appear in process arguments.

External Helm receives no API tokens, user HOME/PATH, kubeconfig or plugin settings. Only an explicit per-instance `helmEnvironment` allow-list is added:

```yaml
instances:
  - id: test
    endpoint: https://argocd.example.invalid
    localHelm: true
    helmBinary: /opt/tools/helm-custom
    helmEnvironment:
      - name: METADATA_HOST
        value: https://metadata.example.invalid
      - name: METADATA_TOKEN
        valueFromEnv: EXAMPLE_METADATA_TOKEN
```

Each entry has exactly one of `value`/`valueFromEnv`; use the latter for secrets. Unset variables exit 4. Init accepts `--helm-env`/`--helm-env-from`. At most 16 unique names and single-line values up to 4096 bytes are accepted. User home/temp/path, Kubernetes, memory-limit and Helm/Diffaro/loader environment names are reserved. Bundled Helm rejects this setting. Reports expose names, never values, and mark external service context as unverified: changes outside Git can affect both renders. Local results always remain `local-context-unverified`.

Temporary chart and values inputs are cleaned up after use.

### Failure and skipped coverage

An unavailable function is not replaced with a guessed value. Safe independent templates may be excluded (up to 16) and the engine retried; surviving targets remain `render_partial`, failed execution, exit 3 and incomplete graph coverage. Matched identities can still compare; one-sided presence stays unknown when the opposite render is incomplete. Helpers, dynamic/shared evaluation and subcharts may prevent isolation. Other malformed input, permission, routing, revision, executable, timeout and limit failures remain errors.

Time/live function probes and repeat-render checks keep nondeterministic fields explicit as `unknownPaths`; differing resource identities skip the Application. `lookup` has no live cluster and returns empty as in ordinary template rendering. Raw errors/arguments are withheld; safe template locations, reason codes and literal chart `fail`/`required` messages can be reported. Resource ordering alone is not a diff; repeated resources/mapping keys use the last occurrence, and Helm hook/test/CRD rules remain those of the selected upstream engine.

### Repo-server

This advanced path requires `grpcurl` and a trusted repo-server route/context. Automatic collection or a managed tunnel also needs `kubectl` and a kubeconfig. It grants a separate permission boundary: the RPC is not assumed to enforce per-Application API RBAC. Keep all context and helpers on operator-managed paths, isolated per tenant/instance.

For automatic context:

```sh
diffaro init --endpoint https://argocd.example.invalid \
  --pr https://github.com/example/charts/pull/123 --single-instance \
  --repo-server-forward --kubeconfig /trusted/kubeconfig --kube-context platform
diffaro doctor --render --pr 123
```

`--repo-server HOST:PORT` instead selects a direct address; `--repo-server-ca` trusts its CA. Plaintext is allowed only on loopback. `--kubectl`/`--grpcurl` select trusted helpers; `--argocd-namespace` defaults to `argocd`. Relative saved paths resolve from the profile directory.

`repoServer` requires `address` or `portForward`, `grpcurl`, and exactly one of `kubernetes` or `contextFile`. A managed tunnel needs Kubernetes context, allows service/deployment/pod targets and ports 1–65535, waits up to 30 seconds and is cleaned up on exit/cancellation. It adds resource reads and `pods/portforward` create, never workload creation. Internal certificate hostname verification is skipped only through that authenticated tunnel, not direct connections.

Automatic context reads Argo CD ConfigMaps, allowed repository/repo-creds Secrets and destination discovery; it does not save them. External/named destinations use the Argo CD discovery cache and need `clusters/get`. Missing capabilities are never replaced with those of the local cluster. Missing tracking settings require the settings API. Exec-auth environments need separate verification.

Alternatively, supply a reviewed `contextFile` JSON from a secret store. It is a fixed, at-most-16-MiB snapshot, not live synchronization:

| Field | Configure |
|---|---|
| `instance`, `contextID`, `argocdNamespace` | Matching instance, safe snapshot label and control namespace |
| `bindings[]` | Exact project, Application namespace, repository and destination; optional allowed Application names |
| `request.repo`, `repos`, `helmRepoCreds` | Read/pull-only Git/dependency credentials allowed by the project; explicit empty arrays if none |
| `kubeVersion`, `apiVersions` | Destination capability snapshot |
| `appLabelKey`, `trackingMethod`, `installationID`, `helmOptions` | Explicit instance tracking/Helm settings |
| `enabledSourceTypes` | Helm only |

No matching binding is denied; ambiguous bindings or an empty name restriction are invalid. Desired specs supply chart values, not the trusted context. AppProject source/destination/namespace checks remain mandatory; unsupported project policies fail explicitly. Credentials go through fixed RPC stdin, without a shell or inherited API token variables. Base/head use the same snapshot and resolved SHAs; response mismatches and context drift fail. Repeated renders mark unknown fields/resources rather than hiding nondeterminism. Reports retain `live-unverified`; custom environments and unverified authentication/OCI/mTLS cases need separate validation.

## Multi-source and external Git values

All sources using the PR repository receive its merge-base/head SHAs, including chart, `$values` and file parameter inputs. Other Git sources use a full commit SHA; local Helm/repo-server can resolve a branch/tag once per run and use that same SHA on both sides. The Application API requires it pinned, and `tagPrefix` sources always require a pinned SHA. The PR SHA never substitutes for another repository.

Source positions/order, selected revisions and known failure positions are retained. A values-only ref produces no resources. The last repeated rendered resource wins without field merging. See [multi-source provenance](reference.md#multi-source-provenance).

## Redaction and limits

Secret payloads and credentials are masked; Secret names/references remain visible. Configure exact JSON pointers for custom secrets:

```yaml
sensitivePaths:
  - /data/connectionString
  - /spec/template/spec/containers/0/env/0/value
```

Pointers include descendants/array indices, not regex or wildcards. Redacted artifacts can still reveal infrastructure; protect their access and retention. See [redaction](reference.md#redaction) and [limit defaults/ranges](reference.md#limits-and-normalization). `render.maxSourceSnapshotBytes` bounds each instance's retained source archives/dependencies; allow room for both revisions. Raising it increases memory use. Inputs and output never become unbounded.

To exclude only proven preexisting failures from deciding the exit, set `failures.excludePreexistingFromExit: true`. It needs `--pr` with `discovery.selection: changedPaths`; elsewhere it has no effect. The [verdict rules](reference.md#how-the-verdict-is-decided) define the effect on status, coverage and policy evaluation.
