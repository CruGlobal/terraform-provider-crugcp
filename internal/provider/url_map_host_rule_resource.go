package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
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
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// writeConflictMaxAttempts caps the read-modify-write retry loop.
//
// Two transient conditions send us round it again, both symptoms of
// another writer touching the same URL map:
//
//   - HTTP 412, when the fingerprint changes between Get and write;
//   - HTTP 400 "The resource ... is not ready", when a previous
//     operation on the map hasn't settled yet.
//
// Eight tries with jittered exponential backoff (200ms doubling, so up
// to ~25s in the worst case) covers a handful of concurrent applies
// without turning a transient race into a 30-minute apply. The caller's
// request_timeout bounds the whole loop regardless.
const writeConflictMaxAttempts = 8

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

func (r *urlMapHostRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_url_map_host_rule"
}

func (r *urlMapHostRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "`crugcp_compute_url_map_host_rule` registers a single host rule (and the matching path matcher that pairs with it) on a shared global Compute URL map. Multiple Terraform configurations can each own one entry on the same URL map without contending over the parent resource.\n\nThe resource manages exactly one `host_rule` block plus one `path_matcher` block, both keyed by `name`. Both are spliced into the URL map's spec under the Compute API's fingerprint-based optimistic locking — concurrent writes from other configurations are retried automatically, whether they surface as a fingerprint conflict or as a not-yet-settled URL map.",
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
				MarkdownDescription: "Resource path of the backend service or serverless NEG to route matching traffic to. Example: `projects/app-stage/regions/us-central1/networkEndpointGroups/serverless-neg`.\n\nSelf-link URLs (`https://www.googleapis.com/compute/v1/...` or `https://compute.googleapis.com/compute/v1/...`) are accepted and stored as the canonical short form so plans stay stable across applies.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"path_rules": schema.SetNestedAttribute{
				MarkdownDescription: "Path rules on this entry's path matcher. Requests whose path matches any pattern in `paths` route to that rule's `service`; unmatched requests fall through to `default_service`. Per Cloud Load Balancing semantics the most specific path wins, so order is not significant — this is an unordered set. Patterns must start with `/` and may use `*` only as a trailing `/*` segment (e.g. `/api` or `/api/*`). Self-link service URLs are canonicalised the same way as `default_service`.",
				Optional:            true,
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

	name := state.Name.ValueString()
	got, err := r.applyEntry(ctx, ref, name, func(m *computepb.UrlMap) (*computepb.UrlMap, error) {
		// Removing a missing entry is a successful no-op. Calling
		// removeEntry unconditionally also normalises ordering so
		// repeated deletes don't trigger surprising payloads.
		return removeEntry(m, name), nil
	})
	if err != nil {
		if isNotFound(err) {
			// Parent gone — the entry is gone too.
			return
		}
		resp.Diagnostics.AddError("Unable to delete URL map host rule", err.Error())
		return
	}

	// Confirm against the post-write read rather than trusting the
	// operation's DONE status. A write that the API accepts but
	// silently drops is a real failure mode here (see needsFullUpdate),
	// and reporting a successful destroy for an entry that is still
	// serving traffic is the worst outcome available: Terraform forgets
	// the resource and nothing ever cleans it up. Create and Update get
	// the equivalent check via updateStateFromURLMap.
	if r.entryStillPresent(ctx, ref, got, name) {
		resp.Diagnostics.AddError(
			"URL map host rule still present after delete",
			fmt.Sprintf("The Compute API reported success but %q is still on %s. The entry has been left in "+
				"Terraform state so a re-run can retry; remove it by hand if it persists.", name, ref),
		)
	}
}

// entryStillPresent decides whether a delete really failed. got is the
// post-write read applyEntry already performed; if that still shows the
// entry, the read is re-taken a couple of times before calling it a
// failure, so a momentarily stale GET can't turn a healthy destroy into
// a spurious error. This cannot mask a genuine problem in the other
// direction — a write the API silently dropped never starts reporting
// the entry as absent.
func (r *urlMapHostRuleResource) entryStillPresent(
	ctx context.Context,
	ref urlMapRef,
	got *computepb.UrlMap,
	name string,
) bool {
	for attempt := 0; ; attempt++ {
		if _, present := findEntry(got, name); !present {
			return false
		}
		if attempt >= 2 {
			return true
		}

		select {
		case <-time.After(500 * time.Millisecond * (1 << attempt)):
		case <-ctx.Done():
			return true
		}

		readCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
		fresh, err := r.cfg.URLMaps.Get(readCtx, &computepb.GetUrlMapRequest{
			Project: ref.Project,
			UrlMap:  ref.Name,
		})
		cancel()
		if err != nil {
			if isNotFound(err) {
				// Parent map vanished; the entry went with it.
				return false
			}
			// Can't re-verify, so trust the read we already have.
			return true
		}
		got = fresh
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
	var (
		lastFingerprint string
		lastErr         error
	)
	for attempt := 0; attempt < writeConflictMaxAttempts; attempt++ {
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

		var op *compute.Operation
		if needsFullUpdate(next) {
			// PUT: the only way to express "this list is now empty".
			// See needsFullUpdate for why PATCH cannot.
			op, err = r.cfg.URLMaps.Update(callCtx, &computepb.UpdateUrlMapRequest{
				Project:        ref.Project,
				UrlMap:         ref.Name,
				UrlMapResource: next,
			})
		} else {
			op, err = r.cfg.URLMaps.Patch(callCtx, &computepb.PatchUrlMapRequest{
				Project:        ref.Project,
				UrlMap:         ref.Name,
				UrlMapResource: next,
			})
		}
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

		if !isRetryableWriteError(err) {
			return nil, err
		}
		lastErr = err

		tflog.Debug(ctx, "retryable write conflict on URL map; retrying", map[string]any{
			"url_map":     ref.String(),
			"name":        name,
			"attempt":     attempt + 1,
			"fingerprint": lastFingerprint,
			"error":       err.Error(),
		})

		// Exponential backoff with full jitter: 200ms, 400ms, 800ms,
		// … doubling per attempt. Bounded by the configured
		// request_timeout outside this function via the caller's
		// context.
		base := 200 * time.Millisecond * (1 << attempt)
		// #nosec G404 -- jitter only; not used for security.
		sleep := time.Duration(rand.Int63n(int64(base)))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("gave up after %d write conflicts on %s; another writer is contending for this URL map: %w", writeConflictMaxAttempts, ref, lastErr)
}

// needsFullUpdate reports whether the desired spec has to be written
// with Update (HTTP PUT) instead of Patch (HTTP PATCH).
//
// urlMaps.patch applies JSON merge patch semantics, where a key absent
// from the request body means "leave this field alone". The generated
// Compute client marshals request bodies with
// protojson.MarshalOptions{AllowPartial: true} — no EmitUnpopulated,
// no EmitDefaultValues — so an empty repeated field is omitted from the
// JSON entirely rather than sent as []. The two behaviours combine into
// a trap: removing the last entry on a URL map produces a body with no
// hostRules and no pathMatchers key, and the API treats that as "change
// nothing". The operation still reports DONE, so the delete looks like
// it succeeded while the host rule is left live. Verified against the
// real API: such a PATCH doesn't even change the URL map's fingerprint.
//
// The marshal options are hard-coded inside the generated client, so
// this can't be fixed by emitting []. urlMaps.update is a PUT — a full
// replace, where an omitted field genuinely means empty — and it still
// honours the fingerprint, so optimistic locking is unaffected.
//
// PATCH is kept for every non-clearing write on purpose. This resource
// splices into a URL map that other configurations also write to, and
// PATCH preserves fields this provider's protobuf doesn't model, where
// a PUT would silently drop them. PUT is therefore used only where
// PATCH is provably incapable of expressing the change.
func needsFullUpdate(next *computepb.UrlMap) bool {
	if next == nil {
		return false
	}
	return len(next.GetHostRules()) == 0 || len(next.GetPathMatchers()) == 0
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

	return entrySpec{
		Name:           plan.Name.ValueString(),
		Hosts:          hosts,
		DefaultService: plan.DefaultService.ValueString(),
		Description:    plan.Description.ValueString(),
		PathRules:      pathRules,
	}, diags
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
	m.DefaultService = types.StringValue(entry.DefaultService)
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
	return diags
}

func buildID(ref urlMapRef, name string) string {
	return fmt.Sprintf("%s/%s", ref, name)
}

// isRetryableWriteError reports whether a failed write is worth another
// trip round the read-modify-write loop. Both conditions mean "another
// writer got here first", which on a deliberately shared URL map is
// routine rather than exceptional.
func isRetryableWriteError(err error) bool {
	return isFingerprintConflict(err) || isResourceNotReady(err)
}

// isResourceNotReady matches HTTP 400 "The resource ... is not ready",
// which Compute returns when a previous operation on the URL map hasn't
// settled yet. Observed in acceptance testing when two resources apply
// against the same map concurrently: without this the apply fails hard
// even though simply waiting would have worked.
//
// The substring is required, not just the status code — 400 is also how
// the API reports genuinely invalid specs, and retrying those would turn
// a clear error into a slow one.
func isResourceNotReady(err error) bool {
	if err == nil {
		return false
	}
	if !strings.Contains(err.Error(), "is not ready") {
		return false
	}
	var apiErr *apierror.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPCode() == 400 {
		return true
	}
	// The REST transport maps 400 to InvalidArgument; accept that too
	// rather than relying on HTTPCode being populated.
	if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
		return true
	}
	return false
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
