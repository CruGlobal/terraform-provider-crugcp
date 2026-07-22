package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fingerprintConflictMaxAttempts caps the read-modify-write retry loop.
// The GCP Compute API returns HTTP 412 when the URL map's fingerprint
// changes between Get and Patch, which is exactly what happens when two
// Terraform runs race against the same URL map. Five tries with
// exponential backoff covers a handful of concurrent applies without
// turning a transient race into a 30-minute apply.
const fingerprintConflictMaxAttempts = 5

var (
	_ resource.Resource                = &urlMapHostRuleResource{}
	_ resource.ResourceWithConfigure   = &urlMapHostRuleResource{}
	_ resource.ResourceWithImportState = &urlMapHostRuleResource{}
)

func NewURLMapHostRuleResource() resource.Resource {
	return &urlMapHostRuleResource{}
}

type urlMapHostRuleResource struct {
	cfg *providerConfig
}

type urlMapHostRuleModel struct {
	ID             types.String `tfsdk:"id"`
	URLMap         types.String `tfsdk:"url_map"`
	Project        types.String `tfsdk:"project"`
	URLMapName     types.String `tfsdk:"url_map_name"`
	Name           types.String `tfsdk:"name"`
	Hosts          types.List   `tfsdk:"hosts"`
	DefaultService types.String `tfsdk:"default_service"`
	Description    types.String `tfsdk:"description"`
	PathRules      types.Set    `tfsdk:"path_rules"`
	RouteRules     types.Set    `tfsdk:"route_rules"`
}

type pathRuleModel struct {
	Paths   types.Set    `tfsdk:"paths"`
	Service types.String `tfsdk:"service"`
}

func pathRuleObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"paths":   types.SetType{ElemType: types.StringType},
		"service": types.StringType,
	}}
}

type routeRuleModel struct {
	Priority types.Int64  `tfsdk:"priority"`
	Match    types.Set    `tfsdk:"match"`
	Service  types.String `tfsdk:"service"`
	Redirect types.Object `tfsdk:"redirect"`
}

type routeMatchModel struct {
	Prefix      types.String `tfsdk:"prefix"`
	FullPath    types.String `tfsdk:"full_path"`
	Headers     types.Set    `tfsdk:"headers"`
	QueryParams types.Set    `tfsdk:"query_params"`
}

type headerMatchModel struct {
	Name    types.String `tfsdk:"name"`
	Regex   types.String `tfsdk:"regex"`
	Present types.Bool   `tfsdk:"present"`
}

type queryParamMatchModel struct {
	Name    types.String `tfsdk:"name"`
	Exact   types.String `tfsdk:"exact"`
	Present types.Bool   `tfsdk:"present"`
}

type redirectModel struct {
	Path         types.String `tfsdk:"path"`
	ResponseCode types.String `tfsdk:"response_code"`
	StripQuery   types.Bool   `tfsdk:"strip_query"`
}

func headerMatchObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"name":    types.StringType,
		"regex":   types.StringType,
		"present": types.BoolType,
	}}
}

func queryParamMatchObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"name":    types.StringType,
		"exact":   types.StringType,
		"present": types.BoolType,
	}}
}

func routeMatchObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"prefix":       types.StringType,
		"full_path":    types.StringType,
		"headers":      types.SetType{ElemType: headerMatchObjectType()},
		"query_params": types.SetType{ElemType: queryParamMatchObjectType()},
	}}
}

func redirectObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"path":          types.StringType,
		"response_code": types.StringType,
		"strip_query":   types.BoolType,
	}}
}

func routeRuleObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"priority": types.Int64Type,
		"match":    types.SetType{ElemType: routeMatchObjectType()},
		"service":  types.StringType,
		"redirect": redirectObjectType(),
	}}
}

func (r *urlMapHostRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_url_map_host_rule"
}

func (r *urlMapHostRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "`crugcp_compute_url_map_host_rule` registers a single host rule (and the matching path matcher that pairs with it) on a shared global Compute URL map. Multiple Terraform configurations can each own one entry on the same URL map without contending over the parent resource.\n\nThe resource manages exactly one `host_rule` block plus one `path_matcher` block, both keyed by `name`. Both are spliced into the URL map's spec via the Compute API's optimistic-locking PATCH semantics — concurrent writes from other configurations are retried automatically on fingerprint conflict.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Composite identifier of the form `projects/{project}/global/urlMaps/{url_map}/{name}`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"url_map": schema.StringAttribute{
				MarkdownDescription: "The global Compute URL map to splice into. Accepts the canonical resource path `projects/{project}/global/urlMaps/{name}` or the equivalent self link. Forces replacement on change — moving an entry between URL maps is destroy-and-recreate.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"project": schema.StringAttribute{
				MarkdownDescription: "Project parsed from `url_map`. Surfaced for downstream interpolations.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"url_map_name": schema.StringAttribute{
				MarkdownDescription: "Short name of the URL map parsed from `url_map`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Unique name for this entry within the URL map. Used as both `host_rule.path_matcher` (the cross-reference) and `path_matcher.name`. Forces replacement on change.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.LengthAtMost(63),
				},
			},
			"hosts": schema.ListAttribute{
				MarkdownDescription: "Hostnames whose requests should be routed to `default_service`. At least one required.",
				Required:            true,
				ElementType:         types.StringType,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
			},
			"default_service": schema.StringAttribute{
				MarkdownDescription: "Resource path of the backend service or serverless NEG to route matching traffic to. Example: `projects/app-stage/regions/us-central1/networkEndpointGroups/serverless-neg`.\n\nSelf-link URLs (`https://www.googleapis.com/compute/v1/...` or `https://compute.googleapis.com/compute/v1/...`) are accepted and stored as the canonical short form so plans stay stable across applies.\n\nOptional when `route_rules` handles all traffic itself (e.g. a catch-all rule); required with `path_rules`, whose unmatched requests fall through to it. At least one of `default_service` or `route_rules` must be set.",
				Optional:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.AtLeastOneOf(path.MatchRoot("route_rules")),
				},
			},
			"path_rules": schema.SetNestedAttribute{
				MarkdownDescription: "Path rules on this entry's path matcher. Requests whose path matches any pattern in `paths` route to that rule's `service`; unmatched requests fall through to `default_service`. Per Cloud Load Balancing semantics the most specific path wins, so order is not significant — this is an unordered set. Patterns must start with `/` and may use `*` only as a trailing `/*` segment (e.g. `/api` or `/api/*`). Self-link service URLs are canonicalised the same way as `default_service`.\n\nMutually exclusive with `route_rules` — a GCP path matcher accepts one or the other, never both. Requires `default_service`.",
				Optional:            true,
				Validators: []validator.Set{
					setvalidator.AlsoRequires(path.MatchRoot("default_service")),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"paths": schema.SetAttribute{
							MarkdownDescription: "Path patterns to match, as an unordered set. At least one required; each must start with `/` and may use `*` only as a trailing `/*` segment (e.g. `/api` or `/api/*`).",
							Required:            true,
							ElementType:         types.StringType,
							Validators: []validator.Set{
								setvalidator.SizeAtLeast(1),
								setvalidator.ValueStringsAre(stringvalidator.RegexMatches(
									regexp.MustCompile(`^/[^*]*$|^/([^*]*/)?\*$`),
									"must start with / and may use * only as a trailing /* segment (e.g. /api or /api/*)",
								)),
							},
						},
						"service": schema.StringAttribute{
							MarkdownDescription: "Resource path of the backend service or serverless NEG to route matching requests to, in the same forms accepted by `default_service`.",
							Required:            true,
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
					},
				},
			},
			"route_rules": schema.SetNestedAttribute{
				MarkdownDescription: "Advanced route rules on this entry's path matcher — required for matching on headers or query parameters (e.g. routing on the presence of an IAP session cookie) and for URL redirects. Mutually exclusive with `path_rules`: a GCP path matcher accepts one or the other, never both.\n\nRules are evaluated in ascending `priority` order and the first match wins — an unordered set with explicit priorities, so config order is not significant. Requests matching none of the rules fall through to `default_service` (if set).\n\nOnly supported on `EXTERNAL_MANAGED` (and `INTERNAL_MANAGED`) load balancers — classic Application Load Balancers reject route rules.",
				Optional:            true,
				Validators: []validator.Set{
					setvalidator.ConflictsWith(path.MatchRoot("path_rules")),
					routeRulePrioritiesUnique{},
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"priority": schema.Int64Attribute{
							MarkdownDescription: "Evaluation order: lower numbers are evaluated first and the first matching rule wins. Must be unique within the entry. 0–2147483647.",
							Required:            true,
							Validators: []validator.Int64{
								int64validator.Between(0, 2147483647),
							},
						},
						"match": schema.SetNestedAttribute{
							MarkdownDescription: "Match conditions for this rule, OR-ed together: the rule fires if any one condition matches. Within a condition, the path match and any `headers` / `query_params` criteria are AND-ed.",
							Required:            true,
							Validators: []validator.Set{
								setvalidator.SizeAtLeast(1),
							},
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"prefix": schema.StringAttribute{
										MarkdownDescription: "Path prefix to match (e.g. `/` or `/signin`). Must start with `/`. Exactly one of `prefix` or `full_path` is required.",
										Optional:            true,
										Validators: []validator.String{
											stringvalidator.RegexMatches(regexp.MustCompile(`^/`), "must start with /"),
											stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("full_path")),
										},
									},
									"full_path": schema.StringAttribute{
										MarkdownDescription: "Exact path to match, after removing query parameters and anchor. Must start with `/`.",
										Optional:            true,
										Validators: []validator.String{
											stringvalidator.RegexMatches(regexp.MustCompile(`^/`), "must start with /"),
										},
									},
									"headers": schema.SetNestedAttribute{
										MarkdownDescription: "Header criteria, all of which must match. Header `regex` matching requires an `EXTERNAL_MANAGED` load balancer.",
										Optional:            true,
										NestedObject: schema.NestedAttributeObject{
											Attributes: map[string]schema.Attribute{
												"name": schema.StringAttribute{
													MarkdownDescription: "Header name to match (e.g. `Cookie`).",
													Required:            true,
													Validators: []validator.String{
														stringvalidator.LengthAtLeast(1),
													},
												},
												"regex": schema.StringAttribute{
													MarkdownDescription: "RE2 regular expression the full header value must match (e.g. `.*GCP_IAA?P_AUTH_TOKEN.*`). Exactly one of `regex` or `present` is required.",
													Optional:            true,
													Validators: []validator.String{
														stringvalidator.LengthAtLeast(1),
														stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("present")),
													},
												},
												"present": schema.BoolAttribute{
													MarkdownDescription: "`true` matches when the header exists regardless of value; `false` matches when it does not exist.",
													Optional:            true,
												},
											},
										},
									},
									"query_params": schema.SetNestedAttribute{
										MarkdownDescription: "Query parameter criteria, all of which must match.",
										Optional:            true,
										NestedObject: schema.NestedAttributeObject{
											Attributes: map[string]schema.Attribute{
												"name": schema.StringAttribute{
													MarkdownDescription: "Query parameter name to match (e.g. `login`).",
													Required:            true,
													Validators: []validator.String{
														stringvalidator.LengthAtLeast(1),
													},
												},
												"exact": schema.StringAttribute{
													MarkdownDescription: "Value the parameter must equal exactly. Exactly one of `exact` or `present` is required.",
													Optional:            true,
													Validators: []validator.String{
														stringvalidator.LengthAtLeast(1),
														stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("present")),
													},
												},
												"present": schema.BoolAttribute{
													MarkdownDescription: "`true` matches when the parameter is present in the request, with or without a value.",
													Optional:            true,
												},
											},
										},
									},
								},
							},
						},
						"service": schema.StringAttribute{
							MarkdownDescription: "Resource path of the backend service or serverless NEG to route matching requests to, in the same forms accepted by `default_service`. Exactly one of `service` or `redirect` is required.",
							Optional:            true,
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
								stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("redirect")),
							},
						},
						"redirect": schema.SingleNestedAttribute{
							MarkdownDescription: "URL redirect to return for matching requests, instead of routing to a backend.",
							Optional:            true,
							Attributes: map[string]schema.Attribute{
								"path": schema.StringAttribute{
									MarkdownDescription: "Path to redirect to (e.g. `/signin`). Must start with `/`.",
									Required:            true,
									Validators: []validator.String{
										stringvalidator.RegexMatches(regexp.MustCompile(`^/`), "must start with /"),
									},
								},
								"response_code": schema.StringAttribute{
									MarkdownDescription: "HTTP status of the redirect. One of `MOVED_PERMANENTLY_DEFAULT` (301), `FOUND` (302), `SEE_OTHER` (303), `TEMPORARY_REDIRECT` (307), `PERMANENT_REDIRECT` (308). Required — prefer a temporary code (`FOUND`) for auth-flow redirects, since browsers cache permanent ones aggressively.",
									Required:            true,
									Validators: []validator.String{
										stringvalidator.OneOf(
											"MOVED_PERMANENTLY_DEFAULT",
											"FOUND",
											"SEE_OTHER",
											"TEMPORARY_REDIRECT",
											"PERMANENT_REDIRECT",
										),
									},
								},
								"strip_query": schema.BoolAttribute{
									MarkdownDescription: "Whether to drop the query string when redirecting.",
									Required:            true,
								},
							},
						},
					},
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-form description written to both the host rule and the path matcher. Optional.",
				Optional:            true,
			},
		},
	}
}

func (r *urlMapHostRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *urlMapHostRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan urlMapHostRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := parseURLMapRef(plan.URLMap.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("url_map"), "Invalid url_map", err.Error())
		return
	}

	spec, diags := planToEntrySpec(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	got, err := r.applyEntry(ctx, ref, spec.Name, func(m *computepb.UrlMap) (*computepb.UrlMap, error) {
		if _, exists := findEntry(m, spec.Name); exists {
			// Two configs racing each other can both hit Create on
			// the same name. Surface the collision rather than
			// silently adopting the other config's entry.
			return nil, fmt.Errorf("an entry named %q already exists on %s; refusing to overwrite", spec.Name, ref)
		}
		return upsertEntry(m, spec), nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to create URL map host rule", err.Error())
		return
	}

	resp.Diagnostics.Append(updateStateFromURLMap(ctx, &plan, ref, got, spec.Name)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *urlMapHostRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state urlMapHostRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := parseURLMapRef(state.URLMap.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("url_map"), "Invalid url_map in state", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	got, err := r.cfg.URLMaps.Get(ctx, &computepb.GetUrlMapRequest{
		Project: ref.Project,
		UrlMap:  ref.Name,
	})
	if err != nil {
		if isNotFound(err) {
			// Parent URL map vanished — the entry is gone with it.
			tflog.Debug(ctx, "parent URL map missing; removing entry from state", map[string]any{"url_map": ref.String()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read URL map", err.Error())
		return
	}

	if _, ok := findEntry(got, state.Name.ValueString()); !ok {
		// Drift: entry removed out-of-band (or never created
		// successfully). Mark for re-creation.
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(updateStateFromURLMap(ctx, &state, ref, got, state.Name.ValueString())...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *urlMapHostRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state urlMapHostRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// url_map and name are RequiresReplace, so on Update they are
	// guaranteed identical between plan and state. Read from plan.
	ref, err := parseURLMapRef(plan.URLMap.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("url_map"), "Invalid url_map", err.Error())
		return
	}
	spec, diags := planToEntrySpec(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	got, err := r.applyEntry(ctx, ref, spec.Name, func(m *computepb.UrlMap) (*computepb.UrlMap, error) {
		return upsertEntry(m, spec), nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to update URL map host rule", err.Error())
		return
	}

	resp.Diagnostics.Append(updateStateFromURLMap(ctx, &plan, ref, got, spec.Name)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *urlMapHostRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state urlMapHostRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := parseURLMapRef(state.URLMap.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("url_map"), "Invalid url_map in state", err.Error())
		return
	}

	_, err = r.applyEntry(ctx, ref, state.Name.ValueString(), func(m *computepb.UrlMap) (*computepb.UrlMap, error) {
		// Removing a missing entry is a successful no-op. Calling
		// removeEntry unconditionally also normalises ordering so
		// repeated deletes don't trigger surprising Patch payloads.
		return removeEntry(m, state.Name.ValueString()), nil
	})
	if err != nil {
		if isNotFound(err) {
			// Parent gone — the entry is gone too.
			return
		}
		resp.Diagnostics.AddError("Unable to delete URL map host rule", err.Error())
		return
	}
}

// ImportState accepts identifiers of the form
// `projects/{project}/global/urlMaps/{url_map}/{name}`. Splitting on
// the last `/` is unambiguous because the canonical url_map path is a
// fixed five-segment string.
func (r *urlMapHostRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	idx := strings.LastIndex(req.ID, "/")
	if idx <= 0 || idx == len(req.ID)-1 {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected `projects/{project}/global/urlMaps/{url_map}/{name}`, got %q", req.ID),
		)
		return
	}
	urlMap := req.ID[:idx]
	name := req.ID[idx+1:]

	ref, err := parseURLMapRef(urlMap)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("url_map"), ref.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), buildID(ref, name))...)
}

// applyEntry runs the read-modify-write loop. mutate receives the
// freshly-fetched URL map and returns the spec to PATCH. Returning an
// error from mutate aborts the loop without retrying — useful when the
// mutation is conditional (e.g. Create refusing to overwrite). 412
// responses from PATCH trigger a backoff and re-fetch.
func (r *urlMapHostRuleResource) applyEntry(
	ctx context.Context,
	ref urlMapRef,
	name string,
	mutate func(*computepb.UrlMap) (*computepb.UrlMap, error),
) (*computepb.UrlMap, error) {
	var lastFingerprint string
	for attempt := 0; attempt < fingerprintConflictMaxAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)

		current, err := r.cfg.URLMaps.Get(callCtx, &computepb.GetUrlMapRequest{
			Project: ref.Project,
			UrlMap:  ref.Name,
		})
		if err != nil {
			cancel()
			return nil, err
		}

		next, mutateErr := mutate(current)
		if mutateErr != nil {
			cancel()
			return nil, mutateErr
		}

		// Preserve the fingerprint from Get; Patch uses it for
		// optimistic locking. proto.Clone in upsertEntry already
		// carries it across, but a mutate callback that returns a
		// brand-new proto would lose it without this guard.
		if next.GetFingerprint() == "" {
			next.Fingerprint = current.Fingerprint
		}
		lastFingerprint = next.GetFingerprint()

		op, err := r.cfg.URLMaps.Patch(callCtx, &computepb.PatchUrlMapRequest{
			Project:        ref.Project,
			UrlMap:         ref.Name,
			UrlMapResource: next,
		})
		if err == nil {
			err = op.Wait(callCtx)
		}
		cancel()

		if err == nil {
			// Re-read to capture the new fingerprint and the
			// authoritative server-side form (proto fields the
			// API may normalise, like trailing-slash on default
			// service paths).
			readCtx, readCancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
			got, getErr := r.cfg.URLMaps.Get(readCtx, &computepb.GetUrlMapRequest{
				Project: ref.Project,
				UrlMap:  ref.Name,
			})
			readCancel()
			if getErr != nil {
				return nil, fmt.Errorf("patch succeeded but post-read failed: %w", getErr)
			}
			return got, nil
		}

		if !isFingerprintConflict(err) {
			return nil, err
		}

		tflog.Debug(ctx, "fingerprint conflict on URL map patch; retrying", map[string]any{
			"url_map":     ref.String(),
			"name":        name,
			"attempt":     attempt + 1,
			"fingerprint": lastFingerprint,
		})

		// Exponential backoff with full jitter: 200ms, 400ms, 800ms,
		// 1.6s, 3.2s. Bounded by the configured request_timeout
		// outside this function via the caller's context.
		base := 200 * time.Millisecond * (1 << attempt)
		// #nosec G404 -- jitter only; not used for security.
		sleep := time.Duration(rand.Int63n(int64(base)))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("gave up after %d fingerprint conflicts on %s; another writer is contending for this URL map", fingerprintConflictMaxAttempts, ref)
}

// planToEntrySpec is a small adapter that pulls the user-facing fields
// off the plan model and into the splice helper's input type.
func planToEntrySpec(ctx context.Context, plan urlMapHostRuleModel) (entrySpec, diag.Diagnostics) {
	var hosts []string
	diags := plan.Hosts.ElementsAs(ctx, &hosts, false)
	if diags.HasError() {
		return entrySpec{}, diags
	}

	// Null/unknown path_rules leaves spec.PathRules nil so the splice
	// helper writes a bare path matcher (and clears any prior rules).
	var pathRules []pathRuleSpec
	if !plan.PathRules.IsNull() && !plan.PathRules.IsUnknown() {
		var models []pathRuleModel
		diags.Append(plan.PathRules.ElementsAs(ctx, &models, false)...)
		if diags.HasError() {
			return entrySpec{}, diags
		}
		pathRules = make([]pathRuleSpec, 0, len(models))
		for _, pm := range models {
			var paths []string
			diags.Append(pm.Paths.ElementsAs(ctx, &paths, false)...)
			if diags.HasError() {
				return entrySpec{}, diags
			}
			pathRules = append(pathRules, pathRuleSpec{
				Paths:   paths,
				Service: pm.Service.ValueString(),
			})
		}
	}

	routeRules, routeDiags := routeRulesFromModel(ctx, plan.RouteRules)
	diags.Append(routeDiags...)
	if diags.HasError() {
		return entrySpec{}, diags
	}

	return entrySpec{
		Name:           plan.Name.ValueString(),
		Hosts:          hosts,
		DefaultService: plan.DefaultService.ValueString(),
		Description:    plan.Description.ValueString(),
		PathRules:      pathRules,
		RouteRules:     routeRules,
	}, diags
}

// routeRulesFromModel unpacks the route_rules set into splice specs.
// A null/unknown set yields nil so the splice helper writes a matcher
// without route rules (and clears any prior ones).
func routeRulesFromModel(ctx context.Context, set types.Set) ([]routeRuleSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	if set.IsNull() || set.IsUnknown() {
		return nil, diags
	}

	var models []routeRuleModel
	diags.Append(set.ElementsAs(ctx, &models, false)...)
	if diags.HasError() {
		return nil, diags
	}

	rules := make([]routeRuleSpec, 0, len(models))
	for _, rm := range models {
		spec := routeRuleSpec{
			// The schema bounds priority to int32 range.
			Priority: int32(rm.Priority.ValueInt64()),
			Service:  rm.Service.ValueString(),
		}

		var matchModels []routeMatchModel
		diags.Append(rm.Match.ElementsAs(ctx, &matchModels, false)...)
		if diags.HasError() {
			return nil, diags
		}
		for _, mm := range matchModels {
			ms := routeMatchSpec{
				Prefix:   mm.Prefix.ValueString(),
				FullPath: mm.FullPath.ValueString(),
			}
			if !mm.Headers.IsNull() && !mm.Headers.IsUnknown() {
				var headerModels []headerMatchModel
				diags.Append(mm.Headers.ElementsAs(ctx, &headerModels, false)...)
				if diags.HasError() {
					return nil, diags
				}
				for _, hm := range headerModels {
					hs := headerMatchSpec{
						Name:  hm.Name.ValueString(),
						Regex: hm.Regex.ValueString(),
					}
					if !hm.Present.IsNull() {
						v := hm.Present.ValueBool()
						hs.Present = &v
					}
					ms.Headers = append(ms.Headers, hs)
				}
			}
			if !mm.QueryParams.IsNull() && !mm.QueryParams.IsUnknown() {
				var paramModels []queryParamMatchModel
				diags.Append(mm.QueryParams.ElementsAs(ctx, &paramModels, false)...)
				if diags.HasError() {
					return nil, diags
				}
				for _, qm := range paramModels {
					qs := queryParamMatchSpec{
						Name:  qm.Name.ValueString(),
						Exact: qm.Exact.ValueString(),
					}
					if !qm.Present.IsNull() {
						v := qm.Present.ValueBool()
						qs.Present = &v
					}
					ms.QueryParams = append(ms.QueryParams, qs)
				}
			}
			spec.Matches = append(spec.Matches, ms)
		}

		if !rm.Redirect.IsNull() && !rm.Redirect.IsUnknown() {
			var rd redirectModel
			diags.Append(rm.Redirect.As(ctx, &rd, basetypes.ObjectAsOptions{})...)
			if diags.HasError() {
				return nil, diags
			}
			spec.Redirect = &redirectSpec{
				Path:         rd.Path.ValueString(),
				ResponseCode: rd.ResponseCode.ValueString(),
				StripQuery:   rd.StripQuery.ValueBool(),
			}
		}

		rules = append(rules, spec)
	}
	return rules, diags
}

// routeRulesToValue is the inverse of routeRulesFromModel: it renders
// splice specs back into the route_rules set for state. Unset optional
// fields become null (never empty strings) so state matches config.
func routeRulesToValue(ctx context.Context, rules []routeRuleSpec) (types.Set, diag.Diagnostics) {
	var diags diag.Diagnostics

	models := make([]routeRuleModel, 0, len(rules))
	for _, r := range rules {
		m := routeRuleModel{
			Priority: types.Int64Value(int64(r.Priority)),
			Service:  stringOrNull(r.Service),
			Redirect: types.ObjectNull(redirectObjectType().AttrTypes),
		}

		if r.Redirect != nil {
			obj, objDiags := types.ObjectValueFrom(ctx, redirectObjectType().AttrTypes, redirectModel{
				Path:         types.StringValue(r.Redirect.Path),
				ResponseCode: stringOrNull(r.Redirect.ResponseCode),
				StripQuery:   types.BoolValue(r.Redirect.StripQuery),
			})
			diags.Append(objDiags...)
			if diags.HasError() {
				return types.SetNull(routeRuleObjectType()), diags
			}
			m.Redirect = obj
		}

		matchModels := make([]routeMatchModel, 0, len(r.Matches))
		for _, ms := range r.Matches {
			mm := routeMatchModel{
				Prefix:      stringOrNull(ms.Prefix),
				FullPath:    stringOrNull(ms.FullPath),
				Headers:     types.SetNull(headerMatchObjectType()),
				QueryParams: types.SetNull(queryParamMatchObjectType()),
			}
			if len(ms.Headers) > 0 {
				headerModels := make([]headerMatchModel, 0, len(ms.Headers))
				for _, h := range ms.Headers {
					headerModels = append(headerModels, headerMatchModel{
						Name:    types.StringValue(h.Name),
						Regex:   stringOrNull(h.Regex),
						Present: types.BoolPointerValue(h.Present),
					})
				}
				headers, headersDiags := types.SetValueFrom(ctx, headerMatchObjectType(), headerModels)
				diags.Append(headersDiags...)
				if diags.HasError() {
					return types.SetNull(routeRuleObjectType()), diags
				}
				mm.Headers = headers
			}
			if len(ms.QueryParams) > 0 {
				paramModels := make([]queryParamMatchModel, 0, len(ms.QueryParams))
				for _, q := range ms.QueryParams {
					paramModels = append(paramModels, queryParamMatchModel{
						Name:    types.StringValue(q.Name),
						Exact:   stringOrNull(q.Exact),
						Present: types.BoolPointerValue(q.Present),
					})
				}
				params, paramsDiags := types.SetValueFrom(ctx, queryParamMatchObjectType(), paramModels)
				diags.Append(paramsDiags...)
				if diags.HasError() {
					return types.SetNull(routeRuleObjectType()), diags
				}
				mm.QueryParams = params
			}
			matchModels = append(matchModels, mm)
		}
		match, matchDiags := types.SetValueFrom(ctx, routeMatchObjectType(), matchModels)
		diags.Append(matchDiags...)
		if diags.HasError() {
			return types.SetNull(routeRuleObjectType()), diags
		}
		m.Match = match

		models = append(models, m)
	}

	set, setDiags := types.SetValueFrom(ctx, routeRuleObjectType(), models)
	diags.Append(setDiags...)
	return set, diags
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// routeRulePrioritiesUnique rejects plans where two route rules share a
// priority. GCP enforces the same invariant server-side, but its error
// arrives only at apply time and doesn't point at the offending rules.
type routeRulePrioritiesUnique struct{}

func (routeRulePrioritiesUnique) Description(context.Context) string {
	return "route rule priorities must be unique within the entry"
}

func (v routeRulePrioritiesUnique) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (routeRulePrioritiesUnique) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := make(map[int64]bool)
	for _, el := range req.ConfigValue.Elements() {
		obj, ok := el.(types.Object)
		if !ok {
			continue
		}
		pr, ok := obj.Attributes()["priority"].(types.Int64)
		if !ok || pr.IsNull() || pr.IsUnknown() {
			continue
		}
		p := pr.ValueInt64()
		if seen[p] {
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Duplicate route rule priority",
				fmt.Sprintf("Priority %d is used by more than one route rule; priorities must be unique within the entry.", p),
			)
		}
		seen[p] = true
	}
}

// updateStateFromURLMap copies authoritative values from the latest
// URL map into the model. It must be called after every successful
// PATCH and after a Read so plan/state stay aligned with the server.
func updateStateFromURLMap(ctx context.Context, m *urlMapHostRuleModel, ref urlMapRef, parent *computepb.UrlMap, name string) diag.Diagnostics {
	var diags diag.Diagnostics
	entry, ok := findEntry(parent, name)
	if !ok {
		diags.AddError(
			"Entry missing from URL map after write",
			fmt.Sprintf("Patched %s but the entry %q was not present in the post-read; another writer may have removed it.", ref, name),
		)
		return diags
	}

	m.URLMap = types.StringValue(ref.String())
	m.Project = types.StringValue(ref.Project)
	m.URLMapName = types.StringValue(ref.Name)
	m.Name = types.StringValue(entry.Name)
	m.DefaultService = stringOrNull(entry.DefaultService)
	m.ID = types.StringValue(buildID(ref, entry.Name))

	hosts, hostsDiag := types.ListValueFrom(ctx, types.StringType, entry.Hosts)
	diags.Append(hostsDiag...)
	if hostsDiag.HasError() {
		return diags
	}
	m.Hosts = hosts

	if entry.Description == "" {
		m.Description = types.StringNull()
	} else {
		m.Description = types.StringValue(entry.Description)
	}

	// Null-preservation: when the API reports no path rules and the
	// incoming model has path_rules null (config never set it), keep it
	// null so we neither perma-diff nor trip "inconsistent result after
	// apply". An explicitly-configured empty set round-trips as an
	// empty set.
	//
	// path_rules and the nested paths are modelled as sets because the
	// Compute API does not guarantee it echoes either back in the order
	// sent (upstream google_compute_url_map models both as sets for the
	// same reason). Set comparison is order-independent, so a differing
	// GET order can't produce a perpetual diff.
	if len(entry.PathRules) == 0 && m.PathRules.IsNull() {
		m.PathRules = types.SetNull(pathRuleObjectType())
	} else {
		models := make([]pathRuleModel, 0, len(entry.PathRules))
		for _, r := range entry.PathRules {
			paths, pathsDiag := types.SetValueFrom(ctx, types.StringType, r.Paths)
			diags.Append(pathsDiag...)
			if pathsDiag.HasError() {
				return diags
			}
			models = append(models, pathRuleModel{
				Paths:   paths,
				Service: types.StringValue(r.Service),
			})
		}
		pathRules, pathRulesDiag := types.SetValueFrom(ctx, pathRuleObjectType(), models)
		diags.Append(pathRulesDiag...)
		if pathRulesDiag.HasError() {
			return diags
		}
		m.PathRules = pathRules
	}

	// Same null-preservation contract as path_rules.
	if len(entry.RouteRules) == 0 && m.RouteRules.IsNull() {
		m.RouteRules = types.SetNull(routeRuleObjectType())
	} else {
		routeRules, routeRulesDiag := routeRulesToValue(ctx, entry.RouteRules)
		diags.Append(routeRulesDiag...)
		if routeRulesDiag.HasError() {
			return diags
		}
		m.RouteRules = routeRules
	}
	return diags
}

func buildID(ref urlMapRef, name string) string {
	return fmt.Sprintf("%s/%s", ref, name)
}

// isFingerprintConflict matches the response shape the Compute API
// uses for "your spec is stale" — HTTP 412 (Precondition Failed) and a
// gRPC FailedPrecondition status alike. We have to accept both because
// the REST transport surfaces 412 via gRPC error mapping.
func isFingerprintConflict(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *apierror.APIError
	if errors.As(err, &apiErr) {
		if apiErr.HTTPCode() == 412 {
			return true
		}
		if apiErr.GRPCStatus() != nil && apiErr.GRPCStatus().Code() == codes.FailedPrecondition {
			return true
		}
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition {
		return true
	}
	// Last-ditch substring match: the message body is stable and
	// hashicorp/google has historically relied on it for the same
	// reason. Cheap to keep as a belt-and-braces fallback.
	return strings.Contains(err.Error(), "fingerprint")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *apierror.APIError
	if errors.As(err, &apiErr) {
		if apiErr.HTTPCode() == 404 {
			return true
		}
		if apiErr.GRPCStatus() != nil && apiErr.GRPCStatus().Code() == codes.NotFound {
			return true
		}
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return true
	}
	return false
}
