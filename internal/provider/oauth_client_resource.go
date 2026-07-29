package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	iam "google.golang.org/api/iam/v1"
)

// apiConsistencyMaxAttempts bounds the wait for the IAM API's reads to
// catch up with its writes. Two distinct lags were measured against
// iam.googleapis.com, both around one second:
//
//   - after Create returns a fully-populated ACTIVE client, a GET or
//     PATCH on that same name still 404s for a few hundred ms — which
//     is exactly where the follow-up placeholder PATCH lands;
//   - after a PATCH returns the updated client, a GET can still report
//     the pre-PATCH values.
//
// The second one is the dangerous one: Terraform refreshes immediately
// after apply, so reading stale values there turns into phantom drift
// on the very next plan. Writes are therefore followed by a poll until
// the API agrees, rather than trusting the write's own response.
//
// Seven attempts with 250ms-doubling backoff waits up to ~16s. In
// practice one retry is enough.
const apiConsistencyMaxAttempts = 7

// oauthClientIDPattern mirrors the server-side rule documented on the
// oauthClientId query parameter: 6–63 chars, lowercase letters, digits
// and hyphens, must start with a letter and must not end with a
// hyphen. Catching it in the plan beats a 400 halfway through apply.
var oauthClientIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,61}[a-z0-9]$`)

var (
	_ resource.Resource                = &iamOAuthClientResource{}
	_ resource.ResourceWithConfigure   = &iamOAuthClientResource{}
	_ resource.ResourceWithImportState = &iamOAuthClientResource{}
)

func NewIAMOAuthClientResource() resource.Resource {
	return &iamOAuthClientResource{}
}

type iamOAuthClientResource struct {
	cfg *providerConfig
}

type iamOAuthClientModel struct {
	ID                    types.String `tfsdk:"id"`
	Project               types.String `tfsdk:"project"`
	Location              types.String `tfsdk:"location"`
	OAuthClientID         types.String `tfsdk:"oauth_client_id"`
	Name                  types.String `tfsdk:"name"`
	ClientID              types.String `tfsdk:"client_id"`
	DisplayName           types.String `tfsdk:"display_name"`
	Description           types.String `tfsdk:"description"`
	Disabled              types.Bool   `tfsdk:"disabled"`
	ClientType            types.String `tfsdk:"client_type"`
	AllowedGrantTypes     types.Set    `tfsdk:"allowed_grant_types"`
	AllowedScopes         types.Set    `tfsdk:"allowed_scopes"`
	AllowedRedirectURIs   types.List   `tfsdk:"allowed_redirect_uris"`
	EffectiveRedirectURIs types.List   `tfsdk:"effective_allowed_redirect_uris"`
	State                 types.String `tfsdk:"state"`
}

func (r *iamOAuthClientResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_iam_oauth_client"
}

func (r *iamOAuthClientResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "`crugcp_iam_oauth_client` manages an IAM OAuth client (`iam.googleapis.com/OauthClient`) — the client Workforce Identity Federation uses to sign users in to IAP.\n\nIt exists to collapse the two-apply dance the upstream `google_iam_oauth_client` forces on you. IAP's redirect URI must embed the client's own server-generated `client_id`, which does not exist until the client has been created, so the documented flow is: create with a dummy URI, read the generated id, edit your config, apply again.\n\nThis resource lets you write the placeholder `{clientid}` anywhere in `allowed_redirect_uris` and resolves it during `Create` — a POST followed immediately by a PATCH, presented to Terraform as one create. One apply, no config edit, no `ignore_changes`, and a real `Read` so out-of-band changes still show up as drift.\n\nSee [hashicorp/terraform-provider-google#22530](https://github.com/hashicorp/terraform-provider-google/issues/22530) and [Configure IAP with workforce identity federation](https://cloud.google.com/iap/docs/use-workforce-identity-federation).\n\n~> **Deleted clients are only soft-deleted.** GCP keeps the name reserved for roughly 30 days after `terraform destroy`, so an immediate re-apply with the same `oauth_client_id` fails. Either pick a new id or `gcloud iam oauth-clients undelete` first.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Full resource name, identical to `name`: `projects/{project}/locations/{location}/oauthClients/{oauth_client_id}`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"project": schema.StringAttribute{
				MarkdownDescription: "Project that owns the OAuth client. Forces replacement on change.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"location": schema.StringAttribute{
				MarkdownDescription: "Location of the OAuth client. `global` is the only location the IAM API supports today, and is the default. Forces replacement on change.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("global"),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"oauth_client_id": schema.StringAttribute{
				MarkdownDescription: "The id you choose for the client, forming the last component of its resource name. 6–63 characters of lowercase letters, digits and hyphens, starting with a letter and not ending with one; the `gcp-` prefix is reserved by Google. Distinct from the server-generated `client_id`. Forces replacement on change.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						oauthClientIDPattern,
						"must be 6–63 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen",
					),
					noReservedOAuthClientIDPrefix{},
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Full resource name of the OAuth client, `projects/{project}/locations/{location}/oauthClients/{oauth_client_id}`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"client_id": schema.StringAttribute{
				MarkdownDescription: "The system-generated OAuth client id — the value substituted for `{clientid}` in `allowed_redirect_uris`, and the one you hand to the workforce pool provider.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"display_name": schema.StringAttribute{
				MarkdownDescription: "Human-readable name shown on the consent screen. Maximum 32 characters.",
				Optional:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtMost(32),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-form description of the client. Maximum 256 characters.",
				Optional:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtMost(256),
				},
			},
			"disabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the client is disabled. A disabled client cannot be used to sign in. Defaults to `false`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"client_type": schema.StringAttribute{
				MarkdownDescription: "`CONFIDENTIAL_CLIENT` (has a secret, managed separately via `google_iam_oauth_client_credential`) or `PUBLIC_CLIENT` (no secret). IAP with workforce identity federation uses `CONFIDENTIAL_CLIENT`. Immutable — forces replacement on change.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.OneOf("CONFIDENTIAL_CLIENT", "PUBLIC_CLIENT"),
				},
			},
			"allowed_grant_types": schema.SetAttribute{
				MarkdownDescription: "OAuth grant types the client may use: `AUTHORIZATION_CODE_GRANT` and/or `REFRESH_TOKEN_GRANT`. Unordered.",
				Required:            true,
				ElementType:         types.StringType,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(
						"AUTHORIZATION_CODE_GRANT",
						"REFRESH_TOKEN_GRANT",
					)),
				},
			},
			"allowed_scopes": schema.SetAttribute{
				MarkdownDescription: "Scopes the client may request during OAuth flows, e.g. `https://www.googleapis.com/auth/cloud-platform`. Unordered.",
				Required:            true,
				ElementType:         types.StringType,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
				},
			},
			"allowed_redirect_uris": schema.ListAttribute{
				MarkdownDescription: "Redirect URIs the authorization flow may return to.\n\nEach entry may contain the literal placeholder `{clientid}`, which is replaced with this client's server-generated `client_id` while the resource is being created. For IAP with workforce identity federation that means the whole configuration is a single line:\n\n```hcl\nallowed_redirect_uris = [\n  \"https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect\",\n]\n```\n\nState keeps the values exactly as written, placeholder and all, so the config and the plan stay readable; `effective_allowed_redirect_uris` shows what was actually sent to GCP. `{clientid}` is the only recognised token — anything else in braces is rejected at plan time.",
				Required:            true,
				ElementType:         types.StringType,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					redirectURIPlaceholdersKnown{},
				},
			},
			"effective_allowed_redirect_uris": schema.ListAttribute{
				MarkdownDescription: "`allowed_redirect_uris` with `{clientid}` resolved — the list as GCP actually holds it. Known only after apply on create, and after any change to `allowed_redirect_uris`.",
				Computed:            true,
				ElementType:         types.StringType,
				PlanModifiers: []planmodifier.List{
					effectiveRedirectURIsPlanModifier{},
				},
			},
			"state": schema.StringAttribute{
				MarkdownDescription: "Server-reported lifecycle state: `ACTIVE`, or `DELETED` while a soft-deleted client is awaiting purge.",
				Computed:            true,
			},
		},
	}
}

func (r *iamOAuthClientResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	cfg, ok := req.ProviderData.(*providerConfig)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *providerConfig, got %T. Please report this to the provider developers.", req.ProviderData),
		)
		return
	}
	r.cfg = cfg
}

// Create runs the two-call dance the upstream resource makes users run
// by hand: POST the client with a provisional redirect URI, then PATCH
// the real one in once the server has minted the client id. Terraform
// sees a single create.
//
// Both follow-up calls retry through the API's read-after-create
// propagation window (see createPropagationMaxAttempts), and Create
// does not return until the client is confirmed readable — otherwise
// the next plan's refresh could 404 and silently propose recreating a
// client that exists.
//
// If a follow-up call fails we still persist state for the client that
// was created — abandoning it would leak a real GCP resource and burn
// the name for 30 days. The saved state deliberately reflects the
// provisional URIs, so the next plan shows the repair as a diff.
func (r *iamOAuthClientResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan iamOAuthClientModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref := oauthClientRef{
		Project:  plan.Project.ValueString(),
		Location: plan.Location.ValueString(),
		ClientID: plan.OAuthClientID.ValueString(),
	}

	configURIs, diags := stringsFromList(ctx, plan.AllowedRedirectURIs)
	resp.Diagnostics.Append(diags...)
	grantTypes, diags := stringsFromSet(ctx, plan.AllowedGrantTypes)
	resp.Diagnostics.Append(diags...)
	scopes, diags := stringsFromSet(ctx, plan.AllowedScopes)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	needsPatch := hasClientIDPlaceholder(configURIs)

	body := &iam.OauthClient{
		AllowedGrantTypes:   grantTypes,
		AllowedScopes:       scopes,
		AllowedRedirectUris: substituteClientID(configURIs, provisionalClientID),
		ClientType:          plan.ClientType.ValueString(),
		Description:         plan.Description.ValueString(),
		DisplayName:         plan.DisplayName.ValueString(),
		Disabled:            plan.Disabled.ValueBool(),
		ForceSendFields:     []string{"Disabled"},
	}

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	created, err := r.cfg.IAM.Projects.Locations.OauthClients.
		Create(ref.Parent(), body).
		OauthClientId(ref.ClientID).
		Context(callCtx).
		Do()
	if err != nil {
		if isAlreadyExists(err) {
			resp.Diagnostics.AddError(
				"OAuth client already exists",
				fmt.Sprintf("%s already exists. GCP reserves the name of a deleted OAuth client for about 30 days, "+
					"so this can also mean a previously destroyed client is still soft-deleted — restore it with "+
					"`gcloud iam oauth-clients undelete %s --project=%s --location=%s` and import it, or choose a "+
					"different oauth_client_id.\n\nUnderlying error: %s",
					ref, ref.ClientID, ref.Project, ref.Location, err),
			)
			return
		}
		resp.Diagnostics.AddError("Unable to create OAuth client", err.Error())
		return
	}

	// saveCreated persists what we know about the client GCP just made,
	// so a failure past this point reports an error without orphaning
	// the resource.
	saveCreated := func() {
		resp.Diagnostics.Append(applyAPIToState(ctx, &plan, ref, created, configURIs)...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}

	if !needsPatch {
		// Nothing to substitute, but still wait for the client to
		// become readable so the next refresh can't 404.
		confirmed, err := r.awaitConsistent(callCtx, ref, body)
		if err != nil {
			saveCreated()
			resp.Diagnostics.AddError(
				"OAuth client created but could not be read back",
				fmt.Sprintf("The client was created and is recorded in state, but reading it back failed. "+
					"Re-run plan to refresh.\n\nUnderlying error: %s", err),
			)
			return
		}
		resp.Diagnostics.Append(applyAPIToState(ctx, &plan, ref, confirmed, configURIs)...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	clientID := created.ClientId
	if clientID == "" {
		// Belt and braces: clientId is output-only and the API does
		// return it on create, but resolving the placeholder against
		// an empty string would silently produce a broken URI.
		got, getErr := r.awaitConsistent(callCtx, ref, body)
		if getErr != nil {
			saveCreated()
			resp.Diagnostics.AddError(
				"OAuth client created but its client_id could not be read",
				fmt.Sprintf("The client exists and has been recorded in state with placeholder redirect URIs still "+
					"unresolved. Re-run apply to finish the update.\n\nUnderlying error: %s", getErr),
			)
			return
		}
		created = got
		clientID = got.ClientId
	}

	tflog.Debug(ctx, "resolving {clientid} placeholder in allowed_redirect_uris", map[string]any{
		"name":      ref.String(),
		"client_id": clientID,
	})

	resolved := substituteClientID(configURIs, clientID)
	_, err = retryOnNotFound(callCtx, "patch", ref, func() (*iam.OauthClient, error) {
		return r.cfg.IAM.Projects.Locations.OauthClients.
			Patch(ref.String(), &iam.OauthClient{AllowedRedirectUris: resolved}).
			UpdateMask("allowedRedirectUris").
			Context(callCtx).
			Do()
	})
	if err != nil {
		saveCreated()
		resp.Diagnostics.AddError(
			"OAuth client created but its redirect URIs could not be resolved",
			fmt.Sprintf("The client was created and is recorded in state, but the follow-up PATCH that substitutes "+
				"the generated client_id into allowed_redirect_uris failed. Its redirect URIs still contain the "+
				"provisional id %s. Re-run apply to repair.\n\nUnderlying error: %s", provisionalClientID, err),
		)
		return
	}

	// Don't trust the PATCH response: wait until a GET agrees, so
	// Terraform's post-apply refresh can't read the pre-PATCH list.
	body.AllowedRedirectUris = resolved
	confirmed, err := r.awaitConsistent(callCtx, ref, body)
	if err != nil {
		saveCreated()
		resp.Diagnostics.AddError(
			"OAuth client created but the API never reported the resolved redirect URIs",
			fmt.Sprintf("The create and the follow-up PATCH both succeeded, but reads never caught up. "+
				"Re-run plan to refresh.\n\nUnderlying error: %s", err),
		)
		return
	}

	resp.Diagnostics.Append(applyAPIToState(ctx, &plan, ref, confirmed, configURIs)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// awaitConsistent polls until a GET reports the values just written.
// See apiConsistencyMaxAttempts for why the write's own response can't
// be taken at face value.
func (r *iamOAuthClientResource) awaitConsistent(
	ctx context.Context,
	ref oauthClientRef,
	want *iam.OauthClient,
) (*iam.OauthClient, error) {
	return pollOAuthClient(ctx, "get", ref,
		func() (*iam.OauthClient, error) {
			return r.cfg.IAM.Projects.Locations.OauthClients.Get(ref.String()).Context(ctx).Do()
		},
		func(got *iam.OauthClient) bool { return matchesWrite(got, want) },
	)
}

// retryOnNotFound runs call, retrying only past the post-create 404
// window. The response is not checked for consistency — callers follow
// up with awaitConsistent.
func retryOnNotFound(
	ctx context.Context,
	what string,
	ref oauthClientRef,
	call func() (*iam.OauthClient, error),
) (*iam.OauthClient, error) {
	return pollOAuthClient(ctx, what, ref, call, func(*iam.OauthClient) bool { return true })
}

// pollOAuthClient re-runs call until it succeeds and settled accepts
// the result. A 404 right after a create means "not propagated yet"
// rather than "gone", so it is retried; any other error is terminal.
func pollOAuthClient(
	ctx context.Context,
	what string,
	ref oauthClientRef,
	call func() (*iam.OauthClient, error),
	settled func(*iam.OauthClient) bool,
) (*iam.OauthClient, error) {
	var lastErr error
	for attempt := 0; attempt < apiConsistencyMaxAttempts; attempt++ {
		if attempt > 0 {
			// 250ms, 500ms, 1s, 2s, 4s, 8s. No jitter: unlike the URL
			// map retry loop this isn't contending with other writers,
			// it's waiting on one resource's own propagation.
			delay := 250 * time.Millisecond * (1 << (attempt - 1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		got, err := call()
		switch {
		case err == nil && settled(got):
			return got, nil
		case err == nil:
			lastErr = fmt.Errorf("the API still reports stale values for %s", ref)
		case isNotFound(err):
			lastErr = err
		default:
			return nil, err
		}

		tflog.Debug(ctx, "IAM API not consistent yet; retrying", map[string]any{
			"name":    ref.String(),
			"call":    what,
			"attempt": attempt + 1,
			"reason":  lastErr.Error(),
		})
	}
	return nil, fmt.Errorf("gave up after %d attempts (~16s): %w", apiConsistencyMaxAttempts, lastErr)
}

// matchesWrite reports whether the API's view has caught up with the
// body that was written. Only the fields this resource sends are
// compared; server-assigned ones (client_id, state) are ignored.
//
// The collections are compared order-insensitively because the API
// makes no promise about echo order.
func matchesWrite(got, want *iam.OauthClient) bool {
	return got.DisplayName == want.DisplayName &&
		got.Description == want.Description &&
		got.Disabled == want.Disabled &&
		sameStringMultiset(got.AllowedRedirectUris, want.AllowedRedirectUris) &&
		sameStringMultiset(got.AllowedGrantTypes, want.AllowedGrantTypes) &&
		sameStringMultiset(got.AllowedScopes, want.AllowedScopes)
}

func (r *iamOAuthClientResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state iamOAuthClientModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := parseOAuthClientRef(state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid OAuth client name in state", err.Error())
		return
	}

	// Prior state holds the config-shaped URIs (placeholder intact);
	// they're the baseline the fresh API values are compared against.
	priorURIs, diags := stringsFromList(ctx, state.AllowedRedirectURIs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	got, err := r.cfg.IAM.Projects.Locations.OauthClients.Get(ref.String()).Context(callCtx).Do()
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read OAuth client", err.Error())
		return
	}

	if got.State == "DELETED" {
		// Soft-deleted out of band. It still reads back for another
		// ~30 days, but it is not usable, so treat it as gone and let
		// Terraform plan a replacement rather than reporting health.
		tflog.Debug(ctx, "OAuth client is soft-deleted; removing from state", map[string]any{"name": ref.String()})
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(applyAPIToState(ctx, &state, ref, got, priorURIs)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *iamOAuthClientResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state iamOAuthClientModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// project, location, oauth_client_id and client_type are all
	// RequiresReplace, so plan and state agree on identity here.
	ref := oauthClientRef{
		Project:  plan.Project.ValueString(),
		Location: plan.Location.ValueString(),
		ClientID: plan.OAuthClientID.ValueString(),
	}

	configURIs, diags := stringsFromList(ctx, plan.AllowedRedirectURIs)
	resp.Diagnostics.Append(diags...)
	grantTypes, diags := stringsFromSet(ctx, plan.AllowedGrantTypes)
	resp.Diagnostics.Append(diags...)
	scopes, diags := stringsFromSet(ctx, plan.AllowedScopes)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The client id is assigned once at create and never changes, so
	// updates can resolve the placeholder straight from state — no
	// second round-trip needed here.
	clientID := state.ClientID.ValueString()
	if hasClientIDPlaceholder(configURIs) && clientID == "" {
		resp.Diagnostics.AddError(
			"Cannot resolve {clientid}",
			"allowed_redirect_uris uses the {clientid} placeholder but client_id is missing from state. "+
				"Re-import the resource (or taint it) so the generated id is known.",
		)
		return
	}

	body := &iam.OauthClient{
		AllowedGrantTypes:   grantTypes,
		AllowedScopes:       scopes,
		AllowedRedirectUris: substituteClientID(configURIs, clientID),
		Description:         plan.Description.ValueString(),
		DisplayName:         plan.DisplayName.ValueString(),
		Disabled:            plan.Disabled.ValueBool(),
		ForceSendFields:     []string{"Disabled"},
	}

	// An explicit mask covers clearing: dropping description from the
	// config sends no description field, and the mask tells the server
	// that omission means "set it empty" rather than "leave alone".
	mask := strings.Join([]string{
		"allowedGrantTypes",
		"allowedRedirectUris",
		"allowedScopes",
		"description",
		"disabled",
		"displayName",
	}, ",")

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	if _, err := r.cfg.IAM.Projects.Locations.OauthClients.
		Patch(ref.String(), body).
		UpdateMask(mask).
		Context(callCtx).
		Do(); err != nil {
		resp.Diagnostics.AddError("Unable to update OAuth client", err.Error())
		return
	}

	// Same read-after-write lag as Create: wait for a GET to agree
	// before handing state back, or the post-apply refresh reports the
	// pre-update values as drift.
	confirmed, err := r.awaitConsistent(callCtx, ref, body)
	if err != nil {
		resp.Diagnostics.AddError(
			"OAuth client updated but the API never reported the new values",
			fmt.Sprintf("The PATCH succeeded but reads never caught up. Re-run plan to refresh."+
				"\n\nUnderlying error: %s", err),
		)
		return
	}

	resp.Diagnostics.Append(applyAPIToState(ctx, &plan, ref, confirmed, configURIs)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete soft-deletes the client. GCP keeps the name reserved for
// roughly 30 days afterwards; that caveat is documented on the schema
// because it bites anyone who destroys and immediately re-applies.
func (r *iamOAuthClientResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state iamOAuthClientModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := parseOAuthClientRef(state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid OAuth client name in state", err.Error())
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	if _, err := r.cfg.IAM.Projects.Locations.OauthClients.Delete(ref.String()).Context(callCtx).Do(); err != nil {
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("Unable to delete OAuth client", err.Error())
	}
}

// ImportState takes the full resource name. Read then fills in the
// rest — including folding the live client id back into `{clientid}`,
// so an imported client's allowed_redirect_uris comes back in the same
// shape a hand-written config would use.
func (r *iamOAuthClientResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ref, err := parseOAuthClientRef(req.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected `projects/{project}/locations/{location}/oauthClients/{oauth_client_id}`: %s", err),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ref.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), ref.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project"), ref.Project)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("location"), ref.Location)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("oauth_client_id"), ref.ClientID)...)
}

// applyAPIToState copies the authoritative API representation into the
// model. configURIs is the config-shaped redirect list — from the plan
// on create/update, from prior state on read — and decides how the
// placeholder round-trips:
//
//   - if the API's list is exactly what those config values resolve to,
//     state keeps the config form, so `{clientid}` survives in state and
//     no phantom diff appears;
//   - otherwise the client really has drifted, and state takes the API's
//     list with the live client id folded back into `{clientid}` so the
//     resulting plan diff is expressed the way the config is written.
func applyAPIToState(ctx context.Context, m *iamOAuthClientModel, ref oauthClientRef, got *iam.OauthClient, configURIs []string) diag.Diagnostics {
	var diags diag.Diagnostics

	m.ID = types.StringValue(ref.String())
	m.Name = types.StringValue(ref.String())
	m.Project = types.StringValue(ref.Project)
	m.Location = types.StringValue(ref.Location)
	m.OAuthClientID = types.StringValue(ref.ClientID)
	m.ClientID = types.StringValue(got.ClientId)
	m.ClientType = types.StringValue(got.ClientType)
	m.Disabled = types.BoolValue(got.Disabled)
	m.State = types.StringValue(got.State)
	m.DisplayName = stringOrNullPreserving(got.DisplayName, m.DisplayName)
	m.Description = stringOrNullPreserving(got.Description, m.Description)

	grantTypes, d := types.SetValueFrom(ctx, types.StringType, got.AllowedGrantTypes)
	diags.Append(d...)
	scopes, d := types.SetValueFrom(ctx, types.StringType, got.AllowedScopes)
	diags.Append(d...)
	effective, d := types.ListValueFrom(ctx, types.StringType, got.AllowedRedirectUris)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	m.AllowedGrantTypes = grantTypes
	m.AllowedScopes = scopes
	m.EffectiveRedirectURIs = effective

	configShaped := configURIs
	if !sameStringMultiset(substituteClientID(configURIs, got.ClientId), got.AllowedRedirectUris) {
		configShaped = unsubstituteClientID(got.AllowedRedirectUris, got.ClientId)
	}
	uris, d := types.ListValueFrom(ctx, types.StringType, configShaped)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	m.AllowedRedirectURIs = uris

	return diags
}

// stringsFromList / stringsFromSet unwrap a collection into a plain
// slice, treating null and unknown as empty.
func stringsFromList(ctx context.Context, l types.List) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if l.IsNull() || l.IsUnknown() {
		return nil, diags
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out, diags
}

func stringsFromSet(ctx context.Context, s types.Set) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if s.IsNull() || s.IsUnknown() {
		return nil, diags
	}
	var out []string
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out, diags
}

// stringOrNullPreserving maps a server-side empty string to null,
// except when the config explicitly set an empty string — returning
// null there would trip "inconsistent result after apply".
func stringOrNullPreserving(apiVal string, current types.String) types.String {
	if apiVal == "" {
		if !current.IsNull() && !current.IsUnknown() && current.ValueString() == "" {
			return types.StringValue("")
		}
		return types.StringNull()
	}
	return types.StringValue(apiVal)
}

// noReservedOAuthClientIDPrefix rejects the `gcp-` prefix, which the
// IAM API reserves for Google's own clients. RE2 has no negative
// lookahead, so this can't fold into oauthClientIDPattern.
type noReservedOAuthClientIDPrefix struct{}

func (noReservedOAuthClientIDPrefix) Description(context.Context) string {
	return "must not start with the Google-reserved prefix gcp-"
}

func (v noReservedOAuthClientIDPrefix) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (noReservedOAuthClientIDPrefix) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if strings.HasPrefix(req.ConfigValue.ValueString(), "gcp-") {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Reserved oauth_client_id prefix",
			"The `gcp-` prefix is reserved for use by Google and cannot be used for your own OAuth clients.",
		)
	}
}

// redirectURIPlaceholdersKnown rejects braced tokens other than
// {clientid}. Without it a typo like {client_id} would sail through
// validation, get posted verbatim, and only surface as a redirect_uri
// mismatch when someone tries to sign in.
type redirectURIPlaceholdersKnown struct{}

func (redirectURIPlaceholdersKnown) Description(context.Context) string {
	return fmt.Sprintf("redirect URIs may only use the %s placeholder", clientIDPlaceholder)
}

func (v redirectURIPlaceholdersKnown) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (redirectURIPlaceholdersKnown) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for i, el := range req.ConfigValue.Elements() {
		s, ok := el.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			continue
		}
		for _, tok := range unknownPlaceholders(s.ValueString()) {
			resp.Diagnostics.AddAttributeError(
				req.Path.AtListIndex(i),
				"Unrecognised placeholder in redirect URI",
				fmt.Sprintf("%q is not a supported placeholder. The only one this resource substitutes is %s, "+
					"which is replaced with the client's generated client_id.", tok, clientIDPlaceholder),
			)
		}
	}
}

// effectiveRedirectURIsPlanModifier marks the resolved list unknown
// whenever allowed_redirect_uris changes. Terraform would otherwise
// carry the prior value forward into the plan and then reject the
// apply for returning something different.
type effectiveRedirectURIsPlanModifier struct{}

func (effectiveRedirectURIsPlanModifier) Description(context.Context) string {
	return "recomputed whenever allowed_redirect_uris changes"
}

func (m effectiveRedirectURIsPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (effectiveRedirectURIsPlanModifier) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	// Create (null state) and destroy (null plan) need no help: the
	// framework already leaves the value unknown / irrelevant.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var planURIs, stateURIs types.List
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("allowed_redirect_uris"), &planURIs)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("allowed_redirect_uris"), &stateURIs)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !planURIs.Equal(stateURIs) {
		resp.PlanValue = types.ListUnknown(types.StringType)
	}
}
