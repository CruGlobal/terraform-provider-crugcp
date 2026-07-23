package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestURLMapHostRuleSchema runs the framework's implementation checks
// over the schema — catching invalid nesting, bad path expressions in
// validators, and default/computed mismatches without a Terraform CLI.
func TestURLMapHostRuleSchema(t *testing.T) {
	ctx := context.Background()

	schemaResponse := &fwresource.SchemaResponse{}
	NewURLMapHostRuleResource().Schema(ctx, fwresource.SchemaRequest{}, schemaResponse)
	if schemaResponse.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResponse.Diagnostics)
	}

	if diags := schemaResponse.Schema.ValidateImplementation(ctx); diags.HasError() {
		t.Fatalf("schema implementation errors: %+v", diags)
	}
}

// TestRouteRules_modelRoundTrip pushes specs through the state
// renderer and back through the plan parser, asserting nothing is
// lost. This is the wiring test for the attr.Type maps: a mismatch
// between the tfsdk-tagged models and the ObjectType helpers fails
// here rather than at apply time.
func TestRouteRules_modelRoundTrip(t *testing.T) {
	ctx := context.Background()
	in := signinRouteRules()

	value, diags := routeRulesToValue(ctx, in)
	if diags.HasError() {
		t.Fatalf("routeRulesToValue: %+v", diags)
	}

	out, diags := routeRulesFromModel(ctx, value)
	if diags.HasError() {
		t.Fatalf("routeRulesFromModel: %+v", diags)
	}
	if len(out) != len(in) {
		t.Fatalf("rule count mismatch: got %d want %d", len(out), len(in))
	}

	byPriority := make(map[int32]routeRuleSpec, len(in))
	for _, r := range in {
		byPriority[r.Priority] = r
	}
	for _, got := range out {
		want, ok := byPriority[got.Priority]
		if !ok {
			t.Fatalf("unexpected priority %d", got.Priority)
		}
		if got.Service != want.Service {
			t.Fatalf("priority %d service mismatch: got %q want %q", got.Priority, got.Service, want.Service)
		}
		if (got.Redirect == nil) != (want.Redirect == nil) {
			t.Fatalf("priority %d redirect presence mismatch", got.Priority)
		}
		if want.Redirect != nil && *got.Redirect != *want.Redirect {
			t.Fatalf("priority %d redirect mismatch: got %+v want %+v", got.Priority, *got.Redirect, *want.Redirect)
		}
		if len(got.Matches) != len(want.Matches) {
			t.Fatalf("priority %d match count mismatch: got %d want %d", got.Priority, len(got.Matches), len(want.Matches))
		}
	}
}

// TestRouteRulePrioritiesUnique exercises the custom set validator
// directly: duplicate priorities are rejected, unique ones pass.
func TestRouteRulePrioritiesUnique(t *testing.T) {
	ctx := context.Background()

	rule := func(priority int64) attr.Value {
		obj, diags := types.ObjectValue(routeRuleObjectType().AttrTypes, map[string]attr.Value{
			"priority": types.Int64Value(priority),
			"match":    types.SetNull(routeMatchObjectType()),
			"service":  types.StringValue("projects/p/global/backendServices/b"),
			"redirect": types.ObjectNull(redirectObjectType().AttrTypes),
		})
		if diags.HasError() {
			t.Fatalf("building rule object: %+v", diags)
		}
		return obj
	}

	set := func(values ...attr.Value) types.Set {
		s, diags := types.SetValue(routeRuleObjectType(), values)
		if diags.HasError() {
			t.Fatalf("building rule set: %+v", diags)
		}
		return s
	}

	cases := []struct {
		name    string
		value   types.Set
		wantErr bool
	}{
		{name: "unique priorities pass", value: set(rule(1), rule(2)), wantErr: false},
		{name: "duplicate priorities fail", value: set(rule(1), rule(1), rule(2)), wantErr: true},
		{name: "null set passes", value: types.SetNull(routeRuleObjectType()), wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.SetResponse{}
			routeRulePrioritiesUnique{}.ValidateSet(ctx, validator.SetRequest{
				Path:        path.Root("route_rules"),
				ConfigValue: tc.value,
			}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("wantErr=%v, got diagnostics: %+v", tc.wantErr, resp.Diagnostics)
			}
		})
	}
}
