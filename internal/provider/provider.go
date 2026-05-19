package provider

import (
	"context"
	"fmt"
	"os"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

// Default HTTP timeout matches the hashicorp/google provider's default.
const defaultRequestTimeoutSeconds = 60

var _ provider.Provider = &crugcpProvider{}

type crugcpProvider struct {
	version string
}

type crugcpProviderModel struct {
	Credentials               types.String `tfsdk:"credentials"`
	AccessToken               types.String `tfsdk:"access_token"`
	ImpersonateServiceAccount types.String `tfsdk:"impersonate_service_account"`
	RequestTimeout            types.String `tfsdk:"request_timeout"`
	RequestReason             types.String `tfsdk:"request_reason"`
}

// providerConfig is what each resource receives via ProviderData. The
// compute clients are constructed once at Configure time and shared
// across resource instances; RequestTimeout governs how long each
// PATCH/Wait round-trip is allowed to take.
type providerConfig struct {
	URLMaps        *compute.UrlMapsClient
	GlobalOps      *compute.GlobalOperationsClient
	RequestTimeout time.Duration
}

// Close releases the gRPC/HTTP connections held by the embedded
// clients. It's safe to call once at provider teardown; the Terraform
// plugin framework doesn't expose a Close hook today, so this exists
// for tests that drive Configure directly.
func (c *providerConfig) Close() {
	if c == nil {
		return
	}
	if c.URLMaps != nil {
		_ = c.URLMaps.Close()
	}
	if c.GlobalOps != nil {
		_ = c.GlobalOps.Close()
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &crugcpProvider{version: version}
	}
}

func (p *crugcpProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "crugcp"
	resp.Version = p.version
}

func (p *crugcpProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The `crugcp` provider exposes individual entries on a shared GCP Compute URL map so multiple Terraform configurations can register host rules and path matchers without conflicting over ownership of the parent URL map.\n\nAuthentication mirrors the `hashicorp/google` provider: by default the provider uses Application Default Credentials, with optional overrides for explicit credentials, a static OAuth access token, or service-account impersonation.",
		Attributes: map[string]schema.Attribute{
			"credentials": schema.StringAttribute{
				MarkdownDescription: "Either the path to a Google Cloud service account JSON key file or the JSON contents of one. Falls back to the `GOOGLE_CREDENTIALS`, `GOOGLE_APPLICATION_CREDENTIALS`, or `GOOGLE_CLOUD_KEYFILE_JSON` environment variables, then to Application Default Credentials.",
				Optional:            true,
				Sensitive:           true,
			},
			"access_token": schema.StringAttribute{
				MarkdownDescription: "A short-lived OAuth 2.0 access token. Mutually exclusive with `credentials` and `impersonate_service_account`. Falls back to the `GOOGLE_OAUTH_ACCESS_TOKEN` environment variable.",
				Optional:            true,
				Sensitive:           true,
			},
			"impersonate_service_account": schema.StringAttribute{
				MarkdownDescription: "The email of a service account to impersonate. The principal supplying credentials must hold `roles/iam.serviceAccountTokenCreator` on the target SA. Falls back to the `GOOGLE_IMPERSONATE_SERVICE_ACCOUNT` environment variable.",
				Optional:            true,
			},
			"request_timeout": schema.StringAttribute{
				MarkdownDescription: "Timeout applied to each underlying Compute API call as a Go `time.Duration` string (for example `\"60s\"`, `\"2m\"`). Defaults to `60s`.",
				Optional:            true,
			},
			"request_reason": schema.StringAttribute{
				MarkdownDescription: "Value sent in the `X-Goog-Request-Reason` header on every API call; visible in GCP audit logs. Falls back to the `CLOUDSDK_CORE_REQUEST_REASON` environment variable.",
				Optional:            true,
			},
		},
	}
}

func (p *crugcpProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg crugcpProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.Credentials.IsUnknown() ||
		cfg.AccessToken.IsUnknown() ||
		cfg.ImpersonateServiceAccount.IsUnknown() ||
		cfg.RequestTimeout.IsUnknown() ||
		cfg.RequestReason.IsUnknown() {
		// Defer to a later plan/apply once unknown values resolve.
		// The framework already passes unknown-config errors back to
		// the user; rejecting here avoids constructing a half-formed
		// client.
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"One or more provider attributes are unknown at Configure time. Resolve the source of the unknown value (use `-target` on the producing resource, or set the value statically) and re-run.",
		)
		return
	}

	credentials := stringValueOrEnv(cfg.Credentials, "GOOGLE_CREDENTIALS",
		"GOOGLE_CLOUD_KEYFILE_JSON",
		"GOOGLE_APPLICATION_CREDENTIALS")
	accessToken := stringValueOrEnv(cfg.AccessToken, "GOOGLE_OAUTH_ACCESS_TOKEN")
	impersonate := stringValueOrEnv(cfg.ImpersonateServiceAccount, "GOOGLE_IMPERSONATE_SERVICE_ACCOUNT")
	requestReason := stringValueOrEnv(cfg.RequestReason, "CLOUDSDK_CORE_REQUEST_REASON")

	timeout := time.Duration(defaultRequestTimeoutSeconds) * time.Second
	if !cfg.RequestTimeout.IsNull() && cfg.RequestTimeout.ValueString() != "" {
		parsed, err := time.ParseDuration(cfg.RequestTimeout.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("request_timeout"),
				"Invalid request_timeout",
				fmt.Sprintf("Expected a Go duration string (e.g. \"60s\"), got %q: %s", cfg.RequestTimeout.ValueString(), err),
			)
			return
		}
		if parsed <= 0 {
			resp.Diagnostics.AddAttributeError(
				path.Root("request_timeout"),
				"Invalid request_timeout",
				fmt.Sprintf("request_timeout must be positive, got %s", parsed),
			)
			return
		}
		timeout = parsed
	}

	if accessToken != "" && (credentials != "" || impersonate != "") {
		// Mirror google provider behaviour: a static access_token
		// short-circuits the credential resolution chain. Refusing
		// the combination spares users from silent overrides.
		resp.Diagnostics.AddError(
			"Conflicting authentication options",
			"`access_token` cannot be combined with `credentials` or `impersonate_service_account`.",
		)
		return
	}

	opts, err := buildClientOptions(ctx, credentials, accessToken, impersonate, requestReason)
	if err != nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("impersonate_service_account"),
			"Unable to set up service account impersonation",
			err.Error(),
		)
		return
	}

	urlMapsClient, err := compute.NewUrlMapsRESTClient(ctx, opts...)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to construct Compute URL Maps client",
			err.Error(),
		)
		return
	}
	opsClient, err := compute.NewGlobalOperationsRESTClient(ctx, opts...)
	if err != nil {
		_ = urlMapsClient.Close()
		resp.Diagnostics.AddError(
			"Unable to construct Compute Global Operations client",
			err.Error(),
		)
		return
	}

	pc := &providerConfig{
		URLMaps:        urlMapsClient,
		GlobalOps:      opsClient,
		RequestTimeout: timeout,
	}
	resp.ResourceData = pc
	resp.DataSourceData = pc
}

func (p *crugcpProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewURLMapHostRuleResource,
	}
}

func (p *crugcpProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

// stringValueOrEnv resolves the value-then-env fallback pattern. The
// env names are checked in order; the first non-empty wins.
func stringValueOrEnv(v types.String, envVars ...string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	for _, name := range envVars {
		if got := os.Getenv(name); got != "" {
			return got
		}
	}
	return ""
}

// buildClientOptions turns the resolved auth knobs into
// google-api/option arguments. The branching is the same shape the
// hashicorp/google provider uses: access_token wins outright; otherwise
// pick up explicit credentials, falling through to ADC; optionally
// wrap the resulting token source with impersonation.
func buildClientOptions(ctx context.Context, credentials, accessToken, impersonateSA, requestReason string) ([]option.ClientOption, error) {
	var opts []option.ClientOption

	const scope = "https://www.googleapis.com/auth/cloud-platform"

	switch {
	case accessToken != "":
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
		opts = append(opts, option.WithTokenSource(ts))

	case credentials != "":
		// Heuristic borrowed from hashicorp/google: a value that
		// parses as JSON is treated as inline credentials; otherwise
		// it's a path on disk. The WithCredentials{File,JSON} options
		// were soft-deprecated in cloud.google.com/go in favour of
		// explicit auth.Credentials objects; we keep them because
		// they match the hashicorp/google provider's UX one-for-one,
		// the security caveat (storing secrets in env or state) is
		// orthogonal to the function call, and the replacement API
		// is still settling.
		if looksLikeJSON(credentials) {
			opts = append(opts, option.WithCredentialsJSON([]byte(credentials))) //nolint:staticcheck // see comment above
		} else {
			opts = append(opts, option.WithCredentialsFile(credentials)) //nolint:staticcheck // see comment above
		}

	default:
		// Application Default Credentials. Don't add an explicit
		// option — google-cloud-go does the right thing.
	}

	if impersonateSA != "" {
		// Build an impersonated token source that wraps whatever
		// principal was selected above. Pass the same option set so
		// the inner exchange uses the explicit credentials when set.
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: impersonateSA,
			Scopes:          []string{scope},
		}, opts...)
		if err != nil {
			return nil, err
		}
		opts = []option.ClientOption{option.WithTokenSource(ts)}
	}

	if requestReason != "" {
		opts = append(opts, option.WithRequestReason(requestReason))
	}

	// Tag outbound calls with a user-agent that identifies this
	// provider. Helpful when triaging in GCP audit logs and matches
	// the hashicorp/google provider's convention.
	opts = append(opts, option.WithUserAgent("terraform-provider-crugcp"))
	return opts, nil
}

func looksLikeJSON(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
