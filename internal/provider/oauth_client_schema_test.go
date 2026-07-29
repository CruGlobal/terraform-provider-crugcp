package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	iam "google.golang.org/api/iam/v1"
)

const testClientID = "287fcecb-aca5-4a51-b9b7-a62149b3adaf"

// TestIAMOAuthClientSchema runs the framework's implementation checks
// over the schema — invalid nesting, bad path expressions in
// validators, defaults on non-computed attributes.
func TestIAMOAuthClientSchema(t *testing.T) {
	ctx := context.Background()

	schemaResponse := &fwresource.SchemaResponse{}
	NewIAMOAuthClientResource().Schema(ctx, fwresource.SchemaRequest{}, schemaResponse)
	if schemaResponse.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResponse.Diagnostics)
	}

	if diags := schemaResponse.Schema.ValidateImplementation(ctx); diags.HasError() {
		t.Fatalf("schema implementation errors: %+v", diags)
	}
}

func TestOAuthClientIDPattern(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{in: "iap-workforce", want: true},
		{in: "abcdef", want: true},         // exactly 6
		{in: "a12345", want: true},         // digits after the leading letter
		{in: "abcde", want: false},         // 5 — too short
		{in: "1abcdef", want: false},       // must start with a letter
		{in: "abcdef-", want: false},       // no trailing hyphen
		{in: "ABCDEF", want: false},        // lowercase only
		{in: "abc_def", want: false},       // no underscores
		{in: "iap workforce", want: false}, // no spaces
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := oauthClientIDPattern.MatchString(tc.in); got != tc.want {
				t.Fatalf("MatchString(%q) = %v want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNoReservedOAuthClientIDPrefix(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{name: "ordinary id", value: types.StringValue("iap-workforce"), wantErr: false},
		{name: "reserved prefix", value: types.StringValue("gcp-something"), wantErr: true},
		{name: "gcp without hyphen is fine", value: types.StringValue("gcpthing"), wantErr: false},
		{name: "null", value: types.StringNull(), wantErr: false},
		{name: "unknown", value: types.StringUnknown(), wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.StringResponse{}
			noReservedOAuthClientIDPrefix{}.ValidateString(ctx, validator.StringRequest{
				Path:        path.Root("oauth_client_id"),
				ConfigValue: tc.value,
			}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("wantErr=%v, diagnostics: %+v", tc.wantErr, resp.Diagnostics)
			}
		})
	}
}

func TestRedirectURIPlaceholdersKnown(t *testing.T) {
	ctx := context.Background()

	list := func(values ...string) types.List {
		elems := make([]attr.Value, 0, len(values))
		for _, v := range values {
			elems = append(elems, types.StringValue(v))
		}
		l, diags := types.ListValue(types.StringType, elems)
		if diags.HasError() {
			t.Fatalf("building list: %+v", diags)
		}
		return l
	}

	cases := []struct {
		name    string
		value   types.List
		wantErr bool
	}{
		{name: "supported placeholder", value: list(iapRedirect), wantErr: false},
		{name: "no placeholder", value: list("https://example.test/cb"), wantErr: false},
		{name: "underscore typo", value: list("https://example.test/{client_id}"), wantErr: true},
		{name: "typo alongside a good entry", value: list(iapRedirect, "https://x.test/{env}"), wantErr: true},
		{name: "null list", value: types.ListNull(types.StringType), wantErr: false},
		{name: "unknown list", value: types.ListUnknown(types.StringType), wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.ListResponse{}
			redirectURIPlaceholdersKnown{}.ValidateList(ctx, validator.ListRequest{
				Path:        path.Root("allowed_redirect_uris"),
				ConfigValue: tc.value,
			}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("wantErr=%v, diagnostics: %+v", tc.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestApplyAPIToState_redirectURIs pins the contract that makes the
// one-apply trick safe: state keeps the config's `{clientid}` spelling
// while the client is in sync, and only switches to API-shaped values
// when something has genuinely drifted.
func TestApplyAPIToState_redirectURIs(t *testing.T) {
	ctx := context.Background()
	ref := oauthClientRef{Project: "p", Location: "global", ClientID: "iap-workforce"}
	resolved := "https://iap.googleapis.com/v1/oauth/clientIds/" + testClientID + ":handleRedirect"

	cases := []struct {
		name          string
		configURIs    []string
		apiURIs       []string
		wantURIs      []string
		wantEffective []string
	}{
		{
			name:          "in sync keeps the placeholder",
			configURIs:    []string{iapRedirect},
			apiURIs:       []string{resolved},
			wantURIs:      []string{iapRedirect},
			wantEffective: []string{resolved},
		},
		{
			name:          "api reordering is not drift",
			configURIs:    []string{iapRedirect, "https://example.test/cb"},
			apiURIs:       []string{"https://example.test/cb", resolved},
			wantURIs:      []string{iapRedirect, "https://example.test/cb"},
			wantEffective: []string{"https://example.test/cb", resolved},
		},
		{
			name:       "out-of-band addition surfaces as drift, folded back to the placeholder",
			configURIs: []string{iapRedirect},
			apiURIs:    []string{resolved, "https://rogue.test/cb"},
			// Folding the live id back into {clientid} means the plan
			// diff reads in the same vocabulary the config uses.
			wantURIs:      []string{iapRedirect, "https://rogue.test/cb"},
			wantEffective: []string{resolved, "https://rogue.test/cb"},
		},
		{
			name:          "import with no prior config adopts the placeholder form",
			configURIs:    nil,
			apiURIs:       []string{resolved},
			wantURIs:      []string{iapRedirect},
			wantEffective: []string{resolved},
		},
		{
			name:       "a create whose follow-up patch failed shows the provisional id",
			configURIs: []string{iapRedirect},
			apiURIs: []string{
				"https://iap.googleapis.com/v1/oauth/clientIds/" + provisionalClientID + ":handleRedirect",
			},
			// Differs from the config value, so the next plan proposes
			// the repair instead of silently declaring success.
			wantURIs: []string{
				"https://iap.googleapis.com/v1/oauth/clientIds/" + provisionalClientID + ":handleRedirect",
			},
			wantEffective: []string{
				"https://iap.googleapis.com/v1/oauth/clientIds/" + provisionalClientID + ":handleRedirect",
			},
		},
		{
			name:          "no placeholder in play is passed through untouched",
			configURIs:    []string{"https://example.test/cb"},
			apiURIs:       []string{"https://example.test/cb"},
			wantURIs:      []string{"https://example.test/cb"},
			wantEffective: []string{"https://example.test/cb"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &iamOAuthClientModel{}
			got := &iam.OauthClient{
				ClientId:            testClientID,
				ClientType:          "CONFIDENTIAL_CLIENT",
				State:               "ACTIVE",
				AllowedGrantTypes:   []string{"AUTHORIZATION_CODE_GRANT"},
				AllowedScopes:       []string{"https://www.googleapis.com/auth/cloud-platform"},
				AllowedRedirectUris: tc.apiURIs,
			}

			diags := applyAPIToState(ctx, m, ref, got, tc.configURIs)
			if diags.HasError() {
				t.Fatalf("applyAPIToState: %+v", diags)
			}

			assertStringList(t, "allowed_redirect_uris", m.AllowedRedirectURIs, tc.wantURIs)
			assertStringList(t, "effective_allowed_redirect_uris", m.EffectiveRedirectURIs, tc.wantEffective)
		})
	}
}

// TestApplyAPIToState_identityAndOptionals covers the non-URI half:
// identity attributes come from the ref (not the payload) so they stay
// stable, and unset optional strings land as null rather than "".
func TestApplyAPIToState_identityAndOptionals(t *testing.T) {
	ctx := context.Background()
	ref := oauthClientRef{Project: "cru-app-stage", Location: "global", ClientID: "iap-workforce"}

	m := &iamOAuthClientModel{}
	got := &iam.OauthClient{
		ClientId:            testClientID,
		ClientType:          "CONFIDENTIAL_CLIENT",
		State:               "ACTIVE",
		Disabled:            false,
		AllowedGrantTypes:   []string{"AUTHORIZATION_CODE_GRANT"},
		AllowedScopes:       []string{"https://www.googleapis.com/auth/cloud-platform"},
		AllowedRedirectUris: []string{"https://example.test/cb"},
	}

	if diags := applyAPIToState(ctx, m, ref, got, []string{"https://example.test/cb"}); diags.HasError() {
		t.Fatalf("applyAPIToState: %+v", diags)
	}

	if want := ref.String(); m.ID.ValueString() != want || m.Name.ValueString() != want {
		t.Fatalf("id/name = %q/%q, want %q", m.ID.ValueString(), m.Name.ValueString(), want)
	}
	if m.Project.ValueString() != ref.Project ||
		m.Location.ValueString() != ref.Location ||
		m.OAuthClientID.ValueString() != ref.ClientID {
		t.Fatalf("identity attributes not taken from the ref: %+v", m)
	}
	if m.ClientID.ValueString() != testClientID {
		t.Fatalf("client_id = %q want %q", m.ClientID.ValueString(), testClientID)
	}
	if !m.DisplayName.IsNull() || !m.Description.IsNull() {
		t.Fatalf("unset optionals should be null, got display_name=%v description=%v", m.DisplayName, m.Description)
	}
	if m.Disabled.ValueBool() {
		t.Fatalf("disabled should be false")
	}
	if m.State.ValueString() != "ACTIVE" {
		t.Fatalf("state = %q want ACTIVE", m.State.ValueString())
	}
}

// TestStringOrNullPreserving guards the one case where "" and null
// must not be conflated: a config that explicitly sets an empty string
// has to read back as an empty string, or the apply is rejected as an
// inconsistent result.
func TestStringOrNullPreserving(t *testing.T) {
	if got := stringOrNullPreserving("", types.StringValue("")); got.IsNull() {
		t.Fatalf("explicit empty string became null")
	}
	if got := stringOrNullPreserving("", types.StringNull()); !got.IsNull() {
		t.Fatalf("unset optional should stay null, got %v", got)
	}
	if got := stringOrNullPreserving("", types.StringUnknown()); !got.IsNull() {
		t.Fatalf("unknown with empty API value should be null, got %v", got)
	}
	if got := stringOrNullPreserving("hello", types.StringNull()); got.ValueString() != "hello" {
		t.Fatalf("API value should win, got %v", got)
	}
}

func assertStringList(t *testing.T, name string, got types.List, want []string) {
	t.Helper()
	if got.IsNull() || got.IsUnknown() {
		t.Fatalf("%s is null/unknown, want %#v", name, want)
	}
	elems := got.Elements()
	if len(elems) != len(want) {
		t.Fatalf("%s has %d entries, want %d (%#v)", name, len(elems), len(want), elems)
	}
	for i, el := range elems {
		s, ok := el.(types.String)
		if !ok {
			t.Fatalf("%s[%d] is not a string: %T", name, i, el)
		}
		if s.ValueString() != want[i] {
			t.Fatalf("%s[%d] = %q want %q", name, i, s.ValueString(), want[i])
		}
	}
}
