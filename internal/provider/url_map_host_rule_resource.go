package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
				MarkdownDescription: "Resource path of the backend service or serverless NEG to route matching traffic to. Example: `projects/app-stage/regions/us-central1/networkEndpointGroups/serverless-neg`.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
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
	return entrySpec{
		Name:           plan.Name.ValueString(),
		Hosts:          hosts,
		DefaultService: plan.DefaultService.ValueString(),
		Description:    plan.Description.ValueString(),
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
