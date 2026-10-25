# Security

Report vulnerabilities through this repository's **Security → Report a vulnerability** option when enabled, or a private contact channel listed on a maintainer's GitHub profile. If neither is available, open a public request for a private reporting channel without vulnerability details or credentials. No response-time guarantee is implied.

Include the Diffaro version, affected command, security boundary, and a minimal reproduction using fictional data.

Use read-only GitHub and Argo CD credentials; the [access table](docs/configuration.md#access) lists backend-specific requirements, including optional repository Secret reads and tunnel permissions. Restrict access to the selected instance. Profiles, kubeconfigs, helper executables, and context files must be trusted operator settings.

An external `helmBinary` runs trusted native code without an OS sandbox. Never select it from PR content. Protect the runner because parameter values may appear in process arguments and an explicit `helmEnvironment` allow-list can supply secrets. The [local Helm configuration](docs/configuration.md#local-helm) defines environment isolation, secret handling and supported authentication.

Security fixes target the default branch before the first release and the newest v0.x release afterward. Upgrade older releases to receive fixes.

Bugs that are not vulnerabilities follow the [public bug-report instructions](README.md#report-a-problem), including review of a public debug bundle before sharing.

Configure [custom sensitive paths](docs/configuration.md#redaction-and-limits) and protect reports even when redaction is enabled: infrastructure metadata can remain visible. Exact masking behavior is in the [redaction contract](docs/reference.md#redaction).

Store credential profiles, private debug bundles and names files in a private directory. Files use POSIX `0600`; do not place them in shared or publicly synchronized locations.
