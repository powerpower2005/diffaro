package domain

// Issue codes (Issue.Code) classify a failure across adapters and engines.
// Every issue and reason code is listed in docs/reference.md; codes_test.go
// enforces it.
const (
	CodeAccessDenied                   = "access_denied"
	CodeAnalysisSkipped                = "analysis_skipped"
	CodeBackendError                   = "backend_error"
	CodeCancelled                      = "cancelled"
	CodeChildIdentityInvalid           = "child_identity_invalid"
	CodeChildSpecInvalid               = "child_spec_invalid"
	CodeClusterCapabilitiesUnavailable = "cluster_capabilities_unavailable"
	CodeConfigurationInvalid           = "configuration_invalid"
	CodeConnectionFailed               = "connection_failed"
	CodeContextDestinationUnavailable  = "context_destination_unavailable"
	CodeContextReadFailed              = "context_read_failed"
	CodeDesiredSpecUnavailable         = "desired_spec_unavailable"
	CodeDestinationInvalid             = "destination_invalid"
	CodeGraphAncestorConflict          = "graph_ancestor_conflict"
	CodeGraphCycle                     = "graph_cycle"
	CodeGraphLimitExceeded             = "graph_limit_exceeded"
	CodeGraphPresenceUnknown           = "graph_presence_unknown"
	CodeGraphSpecConflict              = "graph_spec_conflict"
	CodeHelmExecutionFailed            = "helm_execution_failed"
	CodeHelmFunctionUnavailable        = "helm_function_unavailable"
	CodeIdentityConflict               = "identity_conflict"
	CodeIdentityUnavailable            = "identity_unavailable"
	CodeInternalError                  = "internal_error"
	CodeLimitExceeded                  = "limit_exceeded"
	CodeLocalContextUnavailable        = "local_context_unavailable"
	CodeLocalRenderFailed              = "local_render_failed"
	CodeManifestInvalid                = "manifest_invalid"
	CodeNotFound                       = "not_found"
	CodeRateLimited                    = "rate_limited"
	CodeRenderContextChanged           = "render_context_changed"
	CodeRenderFailed                   = "render_failed"
	CodeRenderNondeterministic         = "render_nondeterministic"
	CodeRenderPartial                  = "render_partial"
	CodeRenderResponseMismatch         = "render_response_mismatch"
	CodeRepoServerFailed               = "repo_server_failed"
	CodeReportLimitExceeded            = "report_limit_exceeded"
	CodeRequestInvalid                 = "request_invalid"
	CodeResponseInvalid                = "response_invalid"
	CodeRevisionInvalid                = "revision_invalid"
	CodeRevisionMappingRequired        = "revision_mapping_required"
	CodeRoutingAmbiguous               = "routing_ambiguous"
	CodeRoutingInvalid                 = "routing_invalid"
	CodeRoutingMissing                 = "routing_missing"
	CodeScopeUnavailable               = "scope_unavailable"
	CodeSelectionAmbiguous             = "selection_ambiguous"
	CodeSelectionChanged               = "selection_changed"
	CodeSelectionEmpty                 = "selection_empty"
	CodeSelectionLimitExceeded         = "selection_limit_exceeded"
	CodeSelectionUnavailable           = "selection_unavailable"
	CodeSourceArchiveFailed            = "source_archive_failed"
	CodeSourceFailed                   = "source_failed"
	CodeSourceInvalid                  = "source_invalid"
	CodeTimeout                        = "timeout"
	CodeUnsupportedApplicationSet      = "unsupported_applicationset"
	CodeUnsupportedIgnoreRules         = "unsupported_ignore_rules"
	CodeUnsupportedProjectPolicy       = "unsupported_project_policy"
	CodeUnsupportedSource              = "unsupported_source"
	CodeUntrustedFork                  = "untrusted_fork"
)

// Reason codes (Issue.ReasonCode) name the cause within a broad issue code,
// such as ReasonChartPathMissing within CodeLocalRenderFailed.
const (
	ReasonAuthenticationRejected = "authentication_rejected"
	ReasonChartMetadataMissing   = "chart_metadata_missing"
	ReasonChartPathMissing       = "chart_path_missing"
	ReasonCredentialMissing      = "credential_missing"
	// Transport causes of connection_failed, and a redirect within
	// backend_error.
	ReasonConnectionClosed  = "connection_closed"
	ReasonConnectionRefused = "connection_refused"
	ReasonDNSFailed         = "dns_failed"
	ReasonRedirected        = "redirected"
	ReasonTLSUntrusted      = "tls_untrusted"
	ReasonInputInvalid      = "input_invalid"
	ReasonParameterInvalid  = "parameter_invalid"
	// ReasonPanic marks an internal_error raised by a recovered panic
	//.
	ReasonPanic                 = "panic"
	ReasonRenderedOutputInvalid = "rendered_output_invalid"
	ReasonRequestForbidden      = "request_forbidden"
	ReasonRevisionMissing       = "revision_missing"
	ReasonServerError           = "server_error"
	// ReasonUnclassified marks an internal_error from an error that no
	// adapter classified; like a panic it is a Diffaro defect.
	ReasonUnclassified     = "unclassified"
	ReasonTemplateFailed   = "template_failed"
	ReasonUnexpectedStatus = "unexpected_status"
	ReasonValueFileMissing = "value_file_missing"
	// ReasonTimeout marks a connection_failed request that timed out; it is
	// classified as transient.
	ReasonTimeout = "timeout"
)
