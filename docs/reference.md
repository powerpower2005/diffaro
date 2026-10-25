# Reference

Use this page to interpret results or build CI integrations after the [README quick start](../README.md#quick-start). Reports use `v1alpha1`: optional compatible fields may be added, while incompatible JSON changes require an explicit schema version. Target diffs do not establish live outcomes. Profile setup is in [configuration](configuration.md).

Look up [commands](#commands), [outputs](#outputs), [exit codes](#exit-codes), [issue/reason codes](#issue-codes), [JSON](#json-report), [selection](#application-selection), [routing](#recursive-graph-and-routing), [limits](#limits-and-normalization), [redaction](#redaction), [policies](#removal-policy) or [compatibility](#backend-compatibility).

## Commands

| Command | Purpose |
|---|---|
| `demo` | Offline embedded fixtures; default/partial/skipped scenarios exit 1/3/1 without certifying an environment |
| `init` | Create a new profile from readable inventory or an offline connection catalog; never overwrite files |
| `validate` | Offline profile validation, no comparison |
| `doctor` | Read-access preflight; `--render` runs the selected target comparison without policy evaluation |
| `diff` | Compare targets and evaluate configured policies |

`diffaro COMMAND --help` lists accepted options without credentials. Unsupported options exit 4. Doctor rejects diff-only output options and PR Markdown. Without revision input, doctor with render compares at one resolved HEAD; without render, it compares nothing. Profiles, modes and token precedence are in [configuration](configuration.md).

### Revisions and PR input

PR input compares **merge-base to head**, not a merge result. Use `--pr NUMBER` or an exact same-repository `https://HOST/OWNER/REPO/pull/NUMBER` URL. Credentials/query/fragment/extra URL paths, fork PRs and mixing PR with `--base/--head` are rejected. The URL never changes trusted API/credential scope. Explicit `--base REF --head REF` resolves remote refs to immutable commits; local unpushed work is not read.

## Outputs

`--format` is `markdown` (default), `terminal`, `json` or diff-only `pr-markdown`. Output goes to stdout unless `--output FILE` is given. `--json-output FILE` accompanies a human format from the same comparison; paths must differ. Stderr carries progress/diagnostics; quiet hides progress, not errors.

Output paths are checked before provider calls. Symlinks/directories and aliased destinations are rejected. Output directories/files use POSIX 0700/0600 permissions. Several outputs are not one atomic transaction. Output/configuration errors can leave no JSON; check stderr/exit code.

### Terminal

Terminal output is bounded at 64 KiB and may shorten long lists/values with a notice. It shows revisions, counts, failures/skips, coverage and grouped resource changes. Control characters are escaped. JSON retains the full evidence; human presentation is not the machine contract.

### PR Markdown

At most 60 KiB, with the stable `<!-- kubeseer:preview -->` marker, resolved SHAs, status/coverage, Application/resource changes, failed/skipped reasons, policies and comparison limitations. Long detail is explicitly shortened. The binary publishes neither comments nor Checks. An integrator must recheck the current head immediately before publication.

### Full Markdown

Shows the full review with explicit bounds and JSON pointers for omitted detail. Changes, failures and unknown coverage remain distinct; unchanged detail can be summarized. The [limits](#limits-and-normalization) describe output bounds.

### Trace

`--trace FILE` writes `diffaro.trace/v1alpha1` diagnostic phases/outcomes and bounded provider request metadata, without values, credentials or raw errors. Timing/order depend on scheduling. It does not change report/exit behavior; keep it private as infrastructure metadata.

### Debug bundle

`--debug-bundle FILE --debug-bundle-level public|private` writes `diffaro.bundle/v1alpha1` with a README and a value-free summary. Public (default) pseudonymizes names and writes a private `.names.json` map. Private retains names and adds redacted report/trace. Set the level explicitly before sharing because profile modes can override it. Paths must differ from other outputs; Files use POSIX 0600 permissions. See [reporting a problem](../README.md#report-a-problem).

Trace/bundle formats are diagnostic and may change between minor versions. Fixes target the newest v0.x release.

## Exit codes

| Code | Meaning | Notes |
|---:|---|---|
| 0 | Successful comparison, no observed changes | Skipped entries keep their reasons; check coverage |
| 1 | Successful comparison, changes found | Not a render failure or policy violation |
| 2 | Configured policy violation | See [removal policy](#removal-policy) |
| 3 | Partial failure or incomplete result | Independent successful or useful incomplete results are retained |
| 4 | Invalid configuration or CLI input | Fix the profile or comparison input |
| 5 | Denied access | No successful comparable target |
| 6 | Overall failure | Render, unsupported input, internal or output error; distinguish causes by `issues[].code` |
| 7 | Cancelled | The caller decides whether to start a new run |

Failures and cancellation take precedence over changes found. In explicit, recursive and repository-discovery runs alike, a run with failed or denied Applications or run-level issues is:

- `partial_failure` when at least one Application result is useful: compared completely (`execution: complete`), skipped with a stated reason (`execution: skipped`), or keeping a resource diff;
- otherwise `denied` when every failure is an access denial, and `failed` when not.

A partial failure can still have `changesFound: true`, and `changesFound: false` alone never means the comparison completed.

### How the verdict is decided

Every report starts with a verdict: the status, the exit code, the failures that were counted, the changes and, when they apply, policies, failures that were not counted and skipped Applications. The same lines appear in PR Markdown, full Markdown, terminal output and console diagnostics.

| Situation | Counted as a failure? | Typical result |
|---|---|---|
| An Application fails at head but rendered at base (`failureOrigin: introduced`) | Always | 3 (or 6 without other results) |
| An Application fails the same way at base and head, and the PR changes none of its inputs (`preexisting`) | Yes, unless `failures.excludePreexistingFromExit` is on | 3; with the opt-in, 1 or 0 when nothing else failed |
| The same failure at base and head, and the PR changes inputs the Application reads (`inputs-changed`) | Always | 3 |
| The same failure at base and head, and whether the PR changes its inputs cannot be determined (`both-sides`) | Always | 3 |
| A failure that may pass on retry (`transient`, `rate_limited`, `server_error`) | Always | 3 or 6; rerun |
| Denied access | Always | 5 when nothing compared, otherwise 3 |
| A run-level issue (selection, inventory, backend setup) | Always | 3 or 6 |
| A policy evaluation that cannot cover a failed Application | Yes, except Applications excluded by the opt-in, which policies do not evaluate | 3; with the opt-in and only excluded failures, 2 on a violation elsewhere, otherwise 1 or 0 |
| A skipped Application (`execution: skipped`) | No; its coverage is incomplete. The verdict names it with its reason in a "Not analyzed" line, and reads `no changes in analyzed Applications` instead of `no changes` | 0 or 1 |

With the profile opt-in `failures.excludePreexistingFromExit: true`:

- It needs `--pr` with `discovery.selection: changedPaths`, which proves unchanged inputs; otherwise the first comparison limit says the setting had no effect.
- A partial failure whose only failures are `preexisting` Applications becomes `success` (exit 0, or 1 with changes). Other failures still count, and a run without any useful result stays `failed` (6).
- Policies do not evaluate the excluded Applications, which neither revision rendered.
- Excluded Applications stay in every report: `preexistingFailuresExcluded` counts them, the verdict has a "Not counted" line, every format states next to the status that they were already failing before the PR and their changes are unknown, and the Errors section shows their origin.

API quota rejections use issue code `rate_limited` (stage `connection`): HTTP 429, or HTTP 403 with `X-RateLimit-Remaining: 0` or `Retry-After`. The reason includes the reset time from `X-RateLimit-Reset` or the `Retry-After` delay when present. It is a transient backend failure, not denied access: exit 6, or 3 when other branches completed. Any other 401/403 remains `access_denied`. What to do is in [troubleshooting](../README.md#troubleshooting).


## Issue codes

Every issue code (`issues[].code`); codes explained with their feature link to it. Each issue carries its own `action`, which is authoritative for that failure; [troubleshooting](../README.md#troubleshooting) covers common causes. Issue and reason codes are a public contract: a code never takes a new meaning and is removed only after a release note marks it deprecated ([support and stability](reference.md#outputs)).

| Code | Stage | Meaning |
|---|---|---|
| `access_denied` | permission | A credential is missing or rejected, a read is forbidden (HTTP 401/403 other than a quota rejection), or no repo-server binding matches. The reason code names the cause. |
| `analysis_skipped` | render/discovery | Console diagnostics only: names a skipped Application with its skip reason. The report records it as `execution: skipped` with `skipReason`. |
| `backend_error` | connection | A backend API answered with a status other than 200, 401, 403, 404 or a quota rejection. Reason code `server_error` (HTTP 5xx, may pass on retry) or `unexpected_status`. |
| `cancelled` | any | The run was cancelled before this step finished. |
| `child_identity_invalid` | discovery | A child Application's name or namespace is not a valid Kubernetes identity. |
| `child_spec_invalid` | discovery | A rendered child Application has no `spec` object. |
| `cluster_capabilities_unavailable` | context | Argo CD cluster discovery for the destination is missing, incomplete or unhealthy. Local Helm skips the Application instead of failing it. |
| `configuration_invalid` | configuration, revision, selection | Trusted configuration or CLI input is invalid. Yields `invalid_configuration` when nothing else ran. |
| `connection_failed` | connection | An API request got no response: network, TLS, proxy or transport timeout. May pass on retry. |
| `context_destination_unavailable` | context | Repo-server context: no reader for the destination cluster's capabilities. |
| `context_read_failed` | context | Repo-server context: reading the Kubernetes render context (Argo CD ConfigMap, repository Secrets, discovery) failed. |
| `desired_spec_unavailable` | render | The default API backend cannot render a new or reconfigured child spec; the child is skipped ([recursive graph](#recursive-graph-and-routing)). |
| `destination_invalid` | selection, context | An Application destination is missing, or does not name exactly one cluster server or name. |
| `graph_ancestor_conflict` | graph | An ancestor has conflicting desired specs, so its descendants are not compared. |
| `graph_cycle` | graph | An Application identity repeats on the active graph path; the reason names the cycle by identities, such as `one/argocd/root -> one/argocd/child -> one/argocd/root`. |
| `graph_limit_exceeded` | graph, lookup | Graph depth (`render.maxDepth`), unique Applications (`render.maxApplications`) or a graph retained-byte budget was exceeded. |
| `graph_presence_unknown` | graph | An Application found on one side only cannot be confirmed added or removed because the opposite graph is incomplete. The reason names the ancestor that was not discovered on that revision when there is one. |
| `graph_spec_conflict` | graph | Several parents define the same child identity with different specs; that node and its descendants are not compared. |
| `helm_execution_failed` | render | The external Helm executable (`helmBinary`) is unusable, or its execution failed. There is no bundled fallback. |
| `helm_function_unavailable` | render | Local Helm: a template calls a function the bundled or external Helm does not provide, such as a server-side custom function ([local Helm](configuration.md#failure-and-skipped-coverage)). |
| `identity_conflict` | manifest | Two manifests describe the same resource identity in a parser that rejects repeats. Comparisons do not produce it; they keep the last occurrence and list repeats. |
| `identity_unavailable` | manifest | A rendered resource lacks `apiVersion`, `kind` or `metadata.name`; `generateName` is not a stable identity. |
| `internal_error` | preview, revision, any | A Diffaro defect. With reason code `panic` it failed only its step or Application; with `unclassified` no adapter classified the error. Report it with a [debug bundle](#debug-bundle), or with `diffaro --version` and the issue's `diagnostic`. |
| `limit_exceeded` | manifest, decode, render, source, context | An input or output exceeded a size, document or resource limit; the reason names the limit. |
| `local_context_unavailable` | render | Cause of a local Helm skip (`execution: skipped`); the skip reason names the source file, API render context or unsupported Helm input that was unavailable. |
| `local_render_failed` | render | Local Helm could not render the chart; the reason code names the cause. Raw template errors are withheld, except a `fail`/`required` message that is a literal of the chart source. |
| `manifest_invalid` | manifest | Rendered output is not valid YAML or uses unsupported YAML: aliases, complex or duplicate keys, a malformed List, an invalid `apiVersion`, NaN or infinity. |
| `not_found` | lookup, revision, render | A requested Application, revision, file or chart path is unavailable (HTTP 404); private objects may be hidden from the token. |
| `rate_limited` | connection | An API quota rejection ([exit codes](#exit-codes)). May pass after the reset. |
| `render_context_changed` | render | The Application, AppProject or repo-server render context changed during the run. May pass on retry. |
| `render_failed` | render | The Argo CD Application API answered a manifest request with a render failure, such as a Helm template error; the reason code names the cause as for `repo_server_failed`, and the message is withheld. Deterministic for `failureOrigin`. A server error without a known render cause stays `backend_error`. |
| `render_nondeterministic` | render | External Helm produced different targets for identical inputs, and the chart names no randomness function. |
| `render_partial` | render | Local Helm rendered only part of the targets because custom functions are unavailable; surviving targets are kept and the Application is `execution: failed` ([local Helm](configuration.md#failure-and-skipped-coverage)). |
| `render_response_mismatch` | render | The repo-server returned a different revision or source type than requested ([repo-server](configuration.md#repo-server)). |
| `repo_server_failed` | render, connection | The repo-server RPC, its subprocess or automatic port forwarding failed ([repo-server](configuration.md#repo-server)). |
| `report_limit_exceeded` | report | Retained comparison results or the final report exceeded their byte budget ([limits](#limits-and-normalization)). |
| `request_invalid` | connection, source | An API request or repository file request could not be built safely. |
| `response_invalid` | decode, lookup, permission, context, capabilities | A backend response is unreadable, not one JSON value, or inconsistent, such as a different Application or cluster identity or a missing project or version. An SSO login page instead of the API gives this code; an HTML response says so in the reason and names the endpoint or `git.apiURL` to check. |
| `revision_invalid` | revision, source | A Git reference or PR number is invalid, or the provider returned no immutable SHA or merge base. |
| `revision_mapping_required` | revision | A non-PR source is not pinned to a full commit SHA ([recursive graph](#recursive-graph-and-routing)). |
| `routing_ambiguous` | routing | Equal-precedence routing rules name different instances for a child ([recursive graph](#recursive-graph-and-routing)). |
| `routing_invalid` | lookup, routing, render, revision | An instance has no renderer, or a backend or Application belongs to another Argo CD instance. |
| `routing_missing` | routing | No routing rule identifies the child's Argo CD instance. |
| `scope_unavailable` | manifest | A rendered resource without a namespace cannot be placed: its kind's scope is unknown, or it is namespaced and the Application has no destination namespace. |
| `selection_ambiguous` | selection | Candidate ownership is cyclic or reverses between revisions, or generated Applications disagree on controller identity ([Application selection](#application-selection)). |
| `selection_changed` | selection | The inventory, generated-Application membership or stored specs changed during the run. May pass on retry. |
| `selection_empty` | selection | No readable generated Applications match the ApplicationSet. |
| `selection_limit_exceeded` | selection | The inventory is paginated or exceeds `render.maxApplications` or its retained-byte budget. |
| `selection_unavailable` | selection | The instance cannot list its Application inventory. |
| `source_archive_failed` | source | A GitHub source archive could not be read safely: links, unsafe paths, truncation or invalid metadata. |
| `source_failed` | render | A multi-source source operation failed without a more specific code; the issue names the source position, repository and revision. |
| `source_invalid` | selection, revision, render | Application sources are malformed (more than 32, a missing repository, invalid refs) or a source plan does not match the Application's sources. |
| `timeout` | any | A stage exceeded `render.timeout`. The reason keeps the step it stopped, such as a request with its host or a local Helm render, and names the configured value, as in `...: API request to HOST timed out; exceeded render.timeout (5s)`; `cancelled` keeps the step the same way. May pass on retry. |
| `unsupported_applicationset` | discovery | An ApplicationSet cannot be expanded by this backend, so graph coverage is incomplete. |
| `unsupported_ignore_rules` | selection | `ignoreDifferences` with `RespectIgnoreDifferences=true` changes what Argo CD applies and is not supported. |
| `unsupported_project_policy` | permission | The AppProject uses a rule or cluster-scoping policy the backend cannot verify ([repo-server](configuration.md#repo-server)). |
| `unsupported_source` | selection, render | The source is not a supported Git Helm source: no explicit `spec.source.helm`, a non-Helm or CMP source, source hydration, or a backend without multi-source rendering. |
| `untrusted_fork` | revision | A cross-repository (fork) pull request is not compared. |

Issues keep `code`; the optional `reasonCode` names the cause within broad codes:

| Reason code | Within | Meaning |
|---|---|---|
| `authentication_rejected` | `access_denied` | HTTP 401: the token was rejected. |
| `chart_metadata_missing` | `local_render_failed`, `repo_server_failed`, `render_failed` | The chart path has no `Chart.yaml` at this revision. |
| `chart_path_missing` | `local_render_failed`, `repo_server_failed`, `not_found` | The chart path does not exist at this revision. |
| `connection_closed` | `connection_failed` | The server or a proxy closed the connection before answering; check the proxy or VPN, then retry. |
| `connection_refused` | `connection_failed` | Nothing accepted the connection at the endpoint's host and port. |
| `credential_missing` | `access_denied` | No token is configured for the request. |
| `dns_failed` | `connection_failed` | The endpoint's host name could not be resolved. |
| `input_invalid` | `local_render_failed` | A Helm input, chart, dependency or manifest is invalid. |
| `panic` | `internal_error` | Diffaro panicked. The reason keeps a Go runtime error message or names the panic value's type; `diagnostic.stack` lists the frames. |
| `parameter_invalid` | `local_render_failed` | A Helm parameter name contains an unescaped `=` or `,`. |
| `redirected` | `backend_error` | HTTP 3xx: the URL names a page or gateway that redirects; Diffaro does not follow redirects. |
| `rendered_output_invalid` | `local_render_failed`, `repo_server_failed`, `render_failed` | A rendered template is not valid manifest output; its content is withheld. |
| `request_forbidden` | `access_denied` | HTTP 403: the token lacks the read. |
| `revision_missing` | `not_found`, `repo_server_failed`, `render_failed` | The Git revision does not exist. |
| `server_error` | `backend_error` | HTTP 5xx; may pass on retry. |
| `template_failed` | `local_render_failed`, `repo_server_failed`, `render_failed` | A Helm template failed. With local Helm, a `fail`/`required` message that is a string literal on the failing line of the chart source is shown as `chart message: "..."`; any other message is withheld. |
| `timeout` | `connection_failed` | The request timed out; may pass on retry. |
| `tls_untrusted` | `connection_failed` | The server's TLS certificate was not accepted, for example because its CA is not trusted on this machine. |
| `unclassified` | `internal_error` | An error no adapter classified reached the report; `diagnostic.chain` names its cause types. |
| `unexpected_status` | `backend_error` | A non-5xx, non-3xx status other than 200, 401, 403 or 404. |
| `value_file_missing` | `local_render_failed`, `repo_server_failed`, `render_failed` | A value file does not exist at this revision. |


## JSON report

| Field | Meaning |
|---|---|
| `schemaVersion` | `v1alpha1` |
| `revisions` | Repository, immutable base/head SHAs and comparison mode. Empty strings when unresolved, as for `doctor` without options or a revision failure. |
| `status` | `success`, `partial_failure`, `invalid_configuration`, `denied`, `failed`, `cancelled` or `policy_violation` |
| `changesFound` | Whether any observed Application, edge or resource addition, change or removal exists. `false` does not mean the comparison succeeded. |
| `coverage` | `explicit-applications`, `connection-preflight`, `recursive-target-graph` or `repository-discovered-target-graph`. A run that fails before rendering still reports the requested mode. |
| `redaction` | `enabled`; not a guarantee that every custom secret was identified |
| `applications` | Global ID (instance/namespace/name), root and `graphPath`, project, cluster and namespace, `state`, `execution`, safe input provenance, resources, issues, `skipReason` |
| `issues` | Structured failures with repository, revision, target, stage, safe reason and action. An optional `diagnostic` classifies the failure without text: `site` (the creating source line), `chain` (cause types, sentinel names and system error numbers), `httpStatus` and `route` (a route template), `contentType` (the media type of a response that could not be decoded, without parameters), and `stack` for a recovered panic as `function directory/file:line`. |
| `limitations` | Applied comparison scope and known uncertainty |
| `discovery`, `selections` | Optional readable-inventory and generated-Application selection evidence |
| `graph` | Optional recursive graph completeness, edges and edge changes |
| `renderBackends` | One entry per profile Argo CD instance of a comparison run, sorted by instance ([render backends](#render-backends)). Absent when no comparison runs, as for `doctor` without `--render`. |
| `policyViolations`, `policy` | Policy ID and target summary strings (empty array without policies) and optional structured evidence; see [removal policy](#removal-policy) |
| `preexistingFailuresExcluded` | Optional count of Applications excluded by `failures.excludePreexistingFromExit` |
| `generator` | `diff` and `doctor`: the build (`version`, `revision`, `goVersion`, `platform`), `command`, the run `mode` and `profile` shape (instance count, backend kinds, discovery selection, names of features in use and of non-default render limits), without hosts, names or values. Absent in `demo`. |

### Render backends

Each `renderBackends` entry has instance, kind, `contextID`, verification, optional version, compatibility, Helm version and SHA-256, and `helmEnvironment` names.

| Field | `argocd-application-api` (default) | `argocd-repo-server` | `local-helm` |
|---|---|---|---|
| `contextID` | `stored-application-spec` | the operator-assigned label of a `contextFile`, or `kubernetes-read-snapshot` for automatic Kubernetes context | from the Helm engine: `helm-<version>/argocd-api-settings-and-capabilities`, or `external-helm/<version>/sha256:<digest>` |
| `verification` | `live-unverified` | `live-unverified` | `local-context-unverified` |
| `compatibility` without `doctor --render` | absent | absent | `bounded-git-helm` (bundled Helm), `bounded-git-helm-external` (external Helm) or `setup-failed` (local Helm that could not be prepared) |

- `contextID` is a label, not a content hash or proof of matching live settings.
- In `demo`, verification is `demo-fixture` and compatibility `demo-only`.
- Every `diff` reads each instance's version API once and records `version`; a failed read is a limitation and changes neither status, exit code nor compatibility.
- After `doctor --render` reads the version API, compatibility is `version-in-tested-matrix`, `version-not-in-tested-matrix` or `version-unavailable` ([backend compatibility](#backend-compatibility)).

### Application and resource states

- Application `state` is `added`, `removed`, `changed`, `unchanged`, `skipped`, `failed`, `denied` or `unknown`; `execution` separately records `complete`, `skipped`, `failed` or `denied`. An Application can be added or changed while its execution failed: its existence or configuration change is confirmed but its final resources could not be rendered.
- With explicit Applications, `changed` means the target resources changed. Recursive mode also counts desired Application spec changes.
- Unknown absence in an incomplete graph never becomes a confirmed removal.
- Resource `state` is `added`, `changed`, `removed`, `unchanged` or `unknown`, and `comparison` is `complete` or `incomplete`.
- A readable Secret payload is compared key by key; a changed key is reported with masked values and `redacted: true`. A payload masked by the Argo CD API stays `comparison: incomplete` and, without another visible change, `state: unknown`; it does not fail the Application. Reports show a hidden replaced value as `value changed (hidden)`; PR Markdown diff blocks show its masked lines as a removed and an added line noted `# value changed (hidden)`.
- Optional `unknownPaths` lists JSON pointers of fields whose value differed between two renders of identical inputs by an external Helm executable, such as a generated password. They are not compared, the comparison is `incomplete`, and a resource without another change is `unknown`.
- An Application rendered from a parent's desired spec that held such a field carries `placeholderInput` (the Application and the marked spec pointers), and so do its descendants. Their resources are `comparison: incomplete`, unchanged ones are `unknown`, and a render failure is reported as a skip.

### Resource changes

- Each change has a JSON pointer `path`, `operation` (`add`, `remove`, `replace`), `before`, `after` and optional `redacted`. `before` is null for add and `after` for remove; in replace, null is an actual null value. A change is retained even when masked values are equal, and equality is never inferred for values the backend already masked.
- Lists whose items are all objects with a unique `name` (containers, env, ports, volumes and similar) are compared item by item; items are also keyed by `alert` or `record` (Prometheus rules) and `secretKey` (ExternalSecret entries). `add`/`replace` paths index the head list and `remove` paths the base list; optional `itemNames` names each list item on the path.
- Lists with unkeyed or duplicate items, reordered shared items, scalar items or a sensitive path stay one change. Human-readable reports may expand them into added and removed items.
- Human-readable reports show multiline string changes as line diffs and may expand large added/removed objects or lists into YAML. These views are bounded; JSON retains the full redacted values.
- Optional `lineHunks` on a `changed` resource view its changes as lines of its redacted YAML. Each hunk has `path` (common path of its changed lines), `baseStart` and `headStart` (1-based line numbers) and `lines` marked ` `, `-` or `+`, with up to 8 lines of context on each side. Hunks are absent for Secrets, for resources with `unknownPaths`, and past their bounds (2,000 lines per side, 120 changed lines, 20 hunks). `changes` stay the compared facts.
- A resource with `comparison: incomplete` carries `incompleteReason`: `argocd-masked-secret` (the Argo CD Application API returned masked Secret values, so whether they changed is unknown; local Helm and repo-server compare them without showing values), `unstable-fields` (fields in `unknownPaths` differed between two renders of identical inputs) or `partial-target` (the other side rendered only partially, so presence there is unknown). Every human format states the reason.
- Resource identity is the instance's cluster scope plus group/kind/namespace/name. An API version change is a change to the same resource's `apiVersion` field. Identical names in different instances are not merged.

### Recursive fields

- `graph.baseComplete`/`headComplete`: whether discovery for each revision completed. Not a statement about Secret diff completeness.
- `graph.baseEdges`/`headEdges`: parent, child, discovery method, parent source repository and revision, routing rule.
- `graph.changes`: `added`, `removed`, `unchanged` or `unknown` per parent/child edge. If the direct parent's discovery completed, edge removal is confirmed regardless of other branch failures. Edge removal does not mean global deletion of the child.
- `applications[].baseTarget`/`headTarget`: repository, revision, `revisionRule`, `requestedRevision` (the branch, tag or `HEAD` a `resolved-source-reference` revision was resolved from at run time; reports name it next to that revision, because the reference can move after the PR), execution, project, cluster/namespace, root/`graphPath` and safe input summary on the side where it was discovered. A shared node's `graphPath` is the first path in ID-sorted DFS; edges hold the other connections.
- `applications[].specChanges`: masked desired Application spec differences.
- `applications[].failureOrigin` (optional, for an Application with both target summaries and a failed side): `introduced` (base renders, head fails), `resolved` (base fails, head renders), `preexisting` (same deterministic causes on both sides and the PR changes none of its inputs), `inputs-changed` (same causes, the PR changes inputs it reads), `both-sides` (same causes, input changes undeterminable) or `transient` (may pass on retry). It does not change states, `changesFound` or the exit code. Markdown reports group failures by cause in an Errors section with origin, Applications, reason, action and effect.
- `applications[].inputChanges` (optional): changed render inputs the Application reads. Inputs are evidence only: they do not say which target field a key produced and do not change states or the exit code.
  - Entries have `kind` (`valueFile`, `chartFile`, `inlineValues`), `repository`, `path`, `state`, `comparison` (`complete`, `incomplete` with `reason`, `path-only`) and key-level `changes` shaped like resource changes. A `chartFile` `state` is the PR file status: `added` or `removed` when the PR adds or removes the file (a rename removes the previous path and adds the new one), otherwise `changed`.
  - File inputs need `--pr` and a complete PR file list, and cover only sources rendered at the PR's base and head; otherwise a limitation says none are listed.
  - When the PR file list read fails, such as a 403 or a rate limit, the first limitation names its code, stage, reason and required action; this never changes the status or the exit code.
  - `inlineValues` keys come from a child's desired `helm.values`, with masked values.
- `applications[].pruning`: resource identities of an Application whose removal is confirmed and whose base render succeeded, with `certainty: conditional` and the limits of the determination. It is a candidate list from the base target, not an execution plan or a guarantee of live deletion: parent prune settings, child finalizers and cascade policy must be checked separately. Finalizers or annotations the parent changes remain in the parent's resource diff.
- `issues[].side`: `base` or `head`. A node's render error appears in that Application and in graph-wide diagnostics.

An Application discovered on only one side is confirmed as added or removed only if the opposite graph is complete; otherwise it is `unknown` and no resource additions or removals are fabricated. Diffs of nodes that succeeded on both sides are preserved.

### Multi-source provenance

- Multi-source comparison preserves source array order. Optional `baseSources`/`headSources` (explicit Applications) and `baseTarget.sources`/`headTarget.sources` (recursive) record per source a 1-based `position`, the original `repository`, optional `ref`, selected `revision`, selection `rule`, `requestedRevision` when the rule is `resolved-source-reference`, and an `inputs` summary without secret values. Single-source output does not gain these fields.
- `inputs.ignoreDifferences` is the optional number of `spec.ignoreDifferences` rules; they are not applied.
- A resource identity repeated within one target uses the last rendered occurrence; fields are not merged. Repeats are listed in `baseRepeatedResources`/`headRepeatedResources` (explicit) or `repeatedResources` (recursive), and reports say the last result wins. Single-source targets follow the same rule.
- Failures with a known source position keep `issues[].sourcePosition` with that source's repository and revision, which are not overwritten with the PR repository context.
- A source with only a ref and no path produces no resources. If one source fails, the Application's incomplete resource set is not returned as a success. The response limit applies to total manifest bytes.

See [multi-source](configuration.md#multi-source-and-external-git-values) for revision rules.


## Application selection

### Repository discovery

`discovery: {mode: repository}` discovers candidates that use the PR repository among readable Applications in the registered instances.

- `projects` is an optional filter; this mode cannot be combined with explicit `applications`/`applicationSets`. `diff` compares recursively without `--recursive`; `doctor` needs `--render`.
- Starting points are selected from each candidate's base/head graphs, and the child specs rendered by the parent are used. If ownership across both revisions forms a cycle or is inverted, the result is `selection_ambiguous`.
- Concurrency and reuse:
  - Candidate probes and the base/head walks run in parallel within `render.maxConcurrency`, plus up to `render.maxConcurrency` background child renders run-wide.
  - Identical-input render results are reused within a run; single-source children pinned to another repository share one render between base and head.
  - Depth, count, byte and stage timeout limits apply to each graph.
- The inventory is checked again after the run. It compares identity, project, source repository and the spec of PR-repository Applications; spec changes in other repositories' Applications do not cause `selection_changed`. Changes or lookup failures are explicit failures.
- Indirect chart dependencies through other repositories are not detected.

The report has `coverage: repository-discovered-target-graph` and `discovery[]` elements with `instance`, `projects`, `status` and `decisions`:

- `status` is `complete`, `no_matches` or `failed`. `no_matches` means no candidates in the readable scope, which differs from a target having no changes.
- A decision holds the Application ID, project, whether it was selected and the reason. An instance whose discovery fails before rendering renders none of its candidates: its decisions are never selected, a repository candidate has reason `same-source-repository-discovery-failed`, and `decisions` is an empty list when the inventory could not be read or was invalid. Optional `inputsUnchanged` is true when the PR changes none of the candidate's own inputs, even if it was rendered because its children are unknown.

`discovery.selection: changedPaths` (opt-in, `--pr` only) skips rendering only what it proves unaffected; unverified path-based skipping is not performed. Profile details are in [configuration](configuration.md#selection-and-routing).

A candidate is not rendered when either holds:

- it is a single Git Helm source in the PR repository that manages no child Applications, and the PR did not change its source path, literal value files, file parameters or `file://` dependency directories, none of which contain symbolic links (`unaffected`);
- its source path has no file at either revision and no PR file under it (`source-missing`).

A child that is not a candidate is skipped in both walks when all hold:

- its parent is a root or has the same spec on both revisions, and renders the child with the same spec at base and head;
- the child manages no child Applications;
- the child has no source in the PR repository, or passes the candidate proof above.

In the report:

- Selected decisions gain `selection` (`rendered`, `unaffected`, `ineligible`, `source-missing`) and `selectionReason`; a skipped child in the readable inventory gets `selection: unaffected` with a reason naming its parent.
- Discovery elements gain an `unaffected` count (candidates not rendered, including `source-missing`) and an `unaffectedDescendants` count (skipped children).
- Unaffected candidates and skipped children do not appear in `applications` and do not affect change detection or the exit code.

### ApplicationSet generated Applications

The profile's optional `applicationSets` has the same instance/namespace/name structure as `applications`; at least one explicit root or ApplicationSet selection is required unless repository discovery is used. `validate` checks syntax only.

- Selection uses the `argoproj.io/v1alpha1` ApplicationSet controller ownerReference (name and UID) in the same namespace from the Application list. Results are limited to current Applications readable under RBAC; no extra ApplicationSet or Kubernetes permission is needed.
- Selected roots are deduplicated and `maxApplications` applies to the total. API list pagination or exceeding the cap is an explicit error.
- Changes in the selection list, owner UID and stored spec before and after the run are detected.
- Template, generator and base/head generated-set comparison are outside the product scope.

JSON `selections[]` holds `source` (ApplicationSet identity), `mode: current-generated-applications`, `ownerUID`, `applications` (selected root IDs) and `boundary`. A failed selection remains with empty `applications` and an issue. `selection_empty`, `selection_ambiguous`, `selection_limit_exceeded`, `selection_unavailable` and `selection_changed` are stage `selection` failures. Independent root results are preserved; a failed or changed selection marks graph discovery incomplete. Permission denial uses `access_denied`.


## Recursive graph and routing

Revision selection follows the [repository mapping rules](configuration.md#multi-source-and-external-git-values). Targets record the rule as `selected-repository-pair` for requested base/head SHAs, `source-commit` for a pinned 40-character SHA, or `resolved-source-reference` for a reference resolved once and reused on both sides. Selected-repository roots and children use the requested SHAs regardless of `source.targetRevision`. When a required pin is missing, the failure is `revision_mapping_required`. No separate Git credential is used for this; the backend reads the commit.

Profile routing precedence (selectors match exactly; equal-precedence rules naming different instances give `routing_ambiguous`; no match gives `routing_missing`):

1. `routing.parents`: `parent: {instance, namespace, name}` and `instance`, a trusted rule for the parent ID.
2. `routing.clusters`: `cluster`, `namespace`, `instance`. `cluster` is the server (preferred) or name of the **parent's destination**, and `namespace` is the child Application CR namespace. It is not inferred from the child's workload destination.
3. `routing.repositories`: `repositoryURL`, `project`, `instance`, a rule for the child source and project.
4. `routing.defaultInstance`: an explicitly configured fallback.

- Repository-generated annotations are not routing authority, and the target instance must be in the profile.
- Identical names in different instances or namespaces are separate nodes. ApplicationSets met during traversal mark it incomplete with `unsupported_applicationset`.
- The stored spec of each root is fixed once at the start of the run; child specs come from the targets the parent renders.
- With the default API backend, a new or reconfigured child is skipped with `desired_spec_unavailable`. A revision override is used only when the whole spec except `targetRevision` equals the installed spec, and drift before and after the request is checked. The stored-child lookup names the desired spec's project, because Argo CD answers a missing Application with 403 when no project is given; a stored child in another project is read again without the project so the spec difference stays visible.
- Each walk visits nodes as a DFS in ID-sorted order; results are merged in sorted order.


## Limits and normalization

Normalization:

- Only `metadata.creationTimestamp`, `resourceVersion`, `uid` and `managedFields` are removed. Status, annotations and array order are not removed or sorted.
- Known namespaced kinds without a namespace inherit the destination namespace. On known cluster-scoped kinds, a namespace is ignored, as Argo CD does. Scopes of CRDs in the same manifest set are used.
- Otherwise the scope Argo CD reports for the Application's managed resources is used; such resources carry `scopeSource: argocd-observed` with a report limitation. An unknown scope without a namespace fails and names the resource.

Profile `render` limits. An omitted field keeps its default; a value outside the supported range is invalid configuration (exit 4).

| Setting | Default | Supported range | Bounds |
|---|---|---|---|
| `render.timeout` | `30s` | `1s`–`10m` | each stage (below) |
| `render.maxConcurrency` | 4 | 1–32 | concurrent renders and lookups |
| `render.maxApplications` | 1,000 | 1–10,000 | Applications in an inventory, selection or graph |
| `render.maxDepth` | 16 | 1–64 (root at 0) | graph depth |
| `render.maxResponseBytes` | 64 MiB (67,108,864) | 1 KiB–64 MiB (1,024–67,108,864 bytes) | each API or manifest response |
| `render.maxDocuments` | 10,000 | 1–10,000 | YAML documents per render, including empty ones |
| `render.maxSourceSnapshotBytes` | 64 MiB | `maxResponseBytes`–512 MiB | local Helm source snapshots per instance ([configuration](configuration.md#redaction-and-limits)) |

- In recursive mode, `maxApplications` counts unique global IDs per revision; long paths into shared subtrees count toward the depth limit.
- `render.timeout` bounds each stage separately, in explicit and graph modes alike: an Application lookup, and one revision's source planning, AppProject check, render and parse. The two revisions of an Application have separate stages.
- YAML aliases, duplicate keys, invalid identities and excessive nesting are rejected; resource counts are bounded. Child names and namespaces are validated as DNS identities before being sent to a provider.
- Recursive mode has a 64 MiB input budget for the root specs and for each snapshot (specs, manifests, edges), with up to 128 diagnostics per revision; omitted diagnostics never mark a graph complete. Raw specs and manifests are held only during the run.
- Accumulated Application results are limited to 64 MiB:
  - Explicit Applications are compared in a rolling window of `render.maxConcurrency` slots. Each Application holds one slot, and its head side renders alongside the base side only while a second slot is free; a limit of 1 keeps every render sequential.
  - Results are offered to the limit in stable ID order, so the retained results and the order of every result and issue do not depend on backend speed. No further Application starts once the limit is reached.
  - Entries past the limit keep only identity, root, graph path and errors, have `state: unknown` and `execution: failed` in every mode, and get `report_limit_exceeded` with guidance to narrow the scope.
- Final reports are bounded at 256 MiB. Full Markdown shortens beyond 200 resources per Application, 50 changes per resource or 4,000 bytes per value and points to JSON.

## Redaction

- Credentials, Secret payloads, Helm inline inputs, credential names, PEM private keys, authorization headers, known tokens and configured `sensitivePaths` are masked. Ordinary strings are not scanned for every embedded secret, so configure organization-specific paths.
- URL userinfo credentials are masked in every string: the password of `scheme://user:password@host`, and an HTTP(S) user without a password, which holds a token, as in `https://token@host`. Scheme, user, host and path stay visible; `ssh://git@host` stays as it is.
- A string a diff run masked in a rendered target (sensitive or configured paths, Secret payloads, decoded Secret data) is also masked where it reappears in another resource's field changes and line hunks and in Application spec changes, when equal or, from 8 bytes, contained in it; the change is marked `redacted`. A string that only contains a masked value is not masked. Helm inline inputs and plugin env of a rendered Application are masked whole as a precaution, not as credentials, so their strings are not masked in other resources: a child's image tag passed by its parent stays visible. In line hunks the key of a `key: value` line and a list marker stay even when the whole line appears inside a masked string.
- A boolean under a credential-like key name, such as a ServiceAccount's `automountServiceAccountToken`, is shown because it holds no credential; under `sensitivePaths` and Helm inputs it stays masked.
- Value file key changes (`inputChanges`) are redacted by sensitive key names, and also mask any string that a diff run masked in a rendered target (sensitive or configured paths, Secret payloads, decoded Secret data), when equal or, from 8 bytes, contained in it.
- `sensitivePaths` are exact JSON pointers relative to the original Kubernetes object, including their sub-paths and array indexes; not regular expressions or wildcards.
- Fields that only name a Secret or one of its keys are shown: `secretKeyRef` name and key, `secretRef.name`, `volumes[].secret.secretName` and its `items[]` key and path, `imagePullSecrets[].name`, Ingress `tls[].secretName`, and ExternalSecret `data[].secretKey` and `remoteRef` key and property. An env var with a sensitive name keeps its literal `value` masked and shows its `valueFrom` reference.
- In value file key changes, `secretName`, `existingSecret` and `remoteRef` list item `key`, `property` and `secretKey` are shown; a list under a sensitive key is compared item by item, an object holding such a reference is redacted field by field and others stay masked whole, and a bare `secretKey` stays masked. `redacted` marks a change whose value redaction partly hid.


## Safe errors

HTTP response bodies, raw error text that may contain URLs and raw fragments of YAML syntax errors are not printed. A recovered panic keeps only a Go runtime error message or the panic value's type, and function names with file lines. A typed failure preserves its cause, but its public message uses only the safe code and reason; its diagnostic names source lines, cause types, standard sentinel errors, system error numbers, HTTP status and route templates, never messages. Endpoint redirects are not followed, to avoid forwarding credentials. HTTPS is required; HTTP is allowed only for loopback test endpoints.


## Removal policy

`diff` evaluates only policies explicitly enabled in the profile. The rule `deny-resource-removal-v1` is violated when a resource in the selected Application's base target is absent from its head target.

```yaml
policies:
  - id: keep-api-deployments
    rule: deny-resource-removal-v1
    match:
      group: apps
      kind: Deployment
      instance: test
      project: demo
      namespace: demo
      name: api
```

- `id` is a stable user-chosen ID. `kind` is required; omitted `group` means the core group. `instance`, `project`, `namespace` and `name` are optional exact matches within the comparison scope; `project` uses the base Application. Wildcards, regular expressions, scripts and manifest-defined policies are not supported.
- The policy profile is a trusted run setting; control where CI reads it from so PRs cannot change operator policies. Policies read only the existing diff and need no extra GitHub, Argo CD or Kubernetes permissions.

| Situation | `policy.evaluation` | Exit code |
|---|---|---|
| Complete comparison, no violations | complete | 0 or 1 |
| Complete comparison, violations | complete | 2 |
| Render failures, skips, unknowns, or policy limits exceeded | incomplete | 3; an overall failure, denial or cancellation keeps its own code |
| Duplicate policy ID, unsupported rule or invalid selector | configuration error before the report | 4 |

- Violations in independently completed Applications are kept on partial failure. Removal is not inferred for failed, skipped or unknown Applications; if nothing could be read, evaluation is incomplete.
- `policyViolations` holds policy IDs and target summaries. The optional `policy` holds the applied rules, `violations`, evaluation scope and limitations. Each violation has the policy and rule IDs, the Application global ID, the resource identity, base/head source revisions (per source for multi-source) and `targetResourcePresent: true → false` evidence. A removed Application's head target is `absent: true`. Secret and ConfigMap content is not included.
- Limits: 32 policies, 16,384 checked resources, 256 violations and 2 MiB of structured evidence. Exceeding one leaves the evaluation incomplete with an action to narrow the scope.
- The policy checks retention in Application targets. It does not guarantee pruning, deletion or downtime, and does not decide whether another Application declares the same resource.


## Backend compatibility

The tested fixture versions are Argo CD v3.0.23, v3.1.16, v3.2.12, v3.3.14, v3.4.9, v3.5.1 and v3.5.3. Tests covered specified Git Helm and graph contexts, not every patch, fresh install or migration, authentication method or organization configuration.

`doctor --render` reads the version API and fills optional `renderBackends[].version` and `compatibility`: `version-in-tested-matrix`, `version-not-in-tested-matrix` or `version-unavailable`. Matrix membership is version evidence, not proof of instance context or a safety certification, so `verification: live-unverified` is kept. A version lookup error stays an explicit issue and lowers a success to a partial failure. Large inventories, all SSH/OIDC/workload-identity environments, actual OCI dependency downloads and direct repo-server client-certificate mTLS are not certified.
