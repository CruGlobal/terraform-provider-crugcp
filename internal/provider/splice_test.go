package provider

import (
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// fixtureUrlMap returns a map pre-populated with one unrelated entry so
// every test exercises the "merge, don't replace" path.
func fixtureUrlMap() *computepb.UrlMap {
	return &computepb.UrlMap{
		Name:        proto.String("internal-shared"),
		Fingerprint: proto.String("fp-0"),
		HostRules: []*computepb.HostRule{
			{
				Hosts:       []string{"other.example.com"},
				PathMatcher: proto.String("other"),
			},
		},
		PathMatchers: []*computepb.PathMatcher{
			{
				Name:           proto.String("other"),
				DefaultService: proto.String("projects/p/regions/r/networkEndpointGroups/other-neg"),
			},
		},
	}
}

func TestUpsertEntry_inserts(t *testing.T) {
	base := fixtureUrlMap()
	out := upsertEntry(base, entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
		Description:    "Hoax stage",
	})

	if len(base.HostRules) != 1 || len(base.PathMatchers) != 1 {
		t.Fatalf("upsertEntry mutated the input — host_rules=%d path_matchers=%d", len(base.HostRules), len(base.PathMatchers))
	}
	if len(out.HostRules) != 2 || len(out.PathMatchers) != 2 {
		t.Fatalf("expected one inserted entry: host_rules=%d path_matchers=%d", len(out.HostRules), len(out.PathMatchers))
	}

	got, ok := findEntry(out, "app-stage")
	if !ok {
		t.Fatalf("inserted entry not found")
	}
	if got.DefaultService != "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg" {
		t.Fatalf("default_service mismatch: %q", got.DefaultService)
	}
	if len(got.Hosts) != 1 || got.Hosts[0] != "app-stage.gcp.cru.org" {
		t.Fatalf("hosts mismatch: %v", got.Hosts)
	}
	if got.Description != "Hoax stage" {
		t.Fatalf("description mismatch: %q", got.Description)
	}
}

func TestUpsertEntry_replaces(t *testing.T) {
	base := fixtureUrlMap()
	base = upsertEntry(base, entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/v1",
	})

	out := upsertEntry(base, entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage-2.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/v2",
	})

	// Replacement, not addition.
	if len(out.HostRules) != 2 || len(out.PathMatchers) != 2 {
		t.Fatalf("expected entry to be replaced, not duplicated: host_rules=%d path_matchers=%d", len(out.HostRules), len(out.PathMatchers))
	}

	got, _ := findEntry(out, "app-stage")
	if got.DefaultService != "projects/app-stage/regions/r/networkEndpointGroups/v2" {
		t.Fatalf("default_service not replaced: %q", got.DefaultService)
	}
	if got.Hosts[0] != "app-stage-2.gcp.cru.org" {
		t.Fatalf("hosts not replaced: %v", got.Hosts)
	}
}

func TestUpsertEntry_preservesUnrelated(t *testing.T) {
	base := fixtureUrlMap()
	out := upsertEntry(base, entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
	})
	other, ok := findEntry(out, "other")
	if !ok {
		t.Fatalf("unrelated entry was dropped")
	}
	if other.Hosts[0] != "other.example.com" {
		t.Fatalf("unrelated entry mutated: %+v", other)
	}
}

func TestRemoveEntry(t *testing.T) {
	base := upsertEntry(fixtureUrlMap(), entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
	})

	out := removeEntry(base, "app-stage")
	if _, ok := findEntry(out, "app-stage"); ok {
		t.Fatalf("entry should have been removed")
	}
	if _, ok := findEntry(out, "other"); !ok {
		t.Fatalf("unrelated entry was dropped during remove")
	}
	if len(out.HostRules) != 1 || len(out.PathMatchers) != 1 {
		t.Fatalf("unexpected slice lengths after remove: host_rules=%d path_matchers=%d", len(out.HostRules), len(out.PathMatchers))
	}
}

func TestRemoveEntry_missingIsNoOp(t *testing.T) {
	base := fixtureUrlMap()
	out := removeEntry(base, "does-not-exist")
	if len(out.HostRules) != 1 || len(out.PathMatchers) != 1 {
		t.Fatalf("missing-entry remove altered slices")
	}
}

func TestCanonicalResourcePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "www.googleapis.com prefix is stripped",
			in:   "https://www.googleapis.com/compute/v1/projects/p/global/backendServices/b",
			want: "projects/p/global/backendServices/b",
		},
		{
			name: "compute.googleapis.com prefix is stripped",
			in:   "https://compute.googleapis.com/compute/v1/projects/p/global/backendServices/b",
			want: "projects/p/global/backendServices/b",
		},
		{
			name: "short form passes through unchanged",
			in:   "projects/p/global/backendServices/b",
			want: "projects/p/global/backendServices/b",
		},
		{
			name: "unknown self-link prefix passes through unchanged",
			in:   "https://other.googleapis.com/v1/projects/p/things/t",
			want: "https://other.googleapis.com/v1/projects/p/things/t",
		},
		{
			name: "empty string passes through",
			in:   "",
			want: "",
		},
		{
			name: "// self link passes through",
			in:   "//compute.googleapis.com/projects/p/global/backendServices/b",
			want: "//compute.googleapis.com/projects/p/global/backendServices/b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalResourcePath(tc.in); got != tc.want {
				t.Fatalf("canonicalResourcePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFindEntry_canonicalisesDefaultService seeds the URL map with the
// self-link form GCP returns on Read and asserts that findEntry hands
// state the canonical short form. This is the wiring test for the
// perma-diff fix; if it regresses, consumers using short paths in
// config will see plan churn after every apply.
func TestFindEntry_canonicalisesDefaultService(t *testing.T) {
	m := &computepb.UrlMap{
		HostRules: []*computepb.HostRule{
			{Hosts: []string{"app.example.com"}, PathMatcher: proto.String("app")},
		},
		PathMatchers: []*computepb.PathMatcher{
			{
				Name:           proto.String("app"),
				DefaultService: proto.String("https://www.googleapis.com/compute/v1/projects/p/global/backendServices/b"),
			},
		},
	}
	got, ok := findEntry(m, "app")
	if !ok {
		t.Fatal("entry not found")
	}
	if got.DefaultService != "projects/p/global/backendServices/b" {
		t.Fatalf("default_service not canonicalised: %q", got.DefaultService)
	}
}

// findMatcher returns the raw PathMatcher owned by name, or nil. Tests
// that assert on the spliced proto (rather than the round-tripped spec)
// use it to reach the PathMatcher's PathRules directly.
func findMatcher(m *computepb.UrlMap, name string) *computepb.PathMatcher {
	for _, p := range m.PathMatchers {
		if p.GetName() == name {
			return p
		}
	}
	return nil
}

func TestUpsertEntry_withPathRules(t *testing.T) {
	out := upsertEntry(fixtureUrlMap(), entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
		PathRules: []pathRuleSpec{
			{
				Paths:   []string{"/api", "/api/*"},
				Service: "projects/app-stage/regions/r/networkEndpointGroups/api-neg",
			},
			{
				Paths:   []string{"/admin/*"},
				Service: "projects/app-stage/regions/r/networkEndpointGroups/admin-neg",
			},
		},
	})

	matcher := findMatcher(out, "app-stage")
	if matcher == nil {
		t.Fatal("path matcher not spliced")
	}
	rules := matcher.GetPathRules()
	if len(rules) != 2 {
		t.Fatalf("expected 2 path rules, got %d", len(rules))
	}

	if got := rules[0].GetPaths(); len(got) != 2 || got[0] != "/api" || got[1] != "/api/*" {
		t.Fatalf("rule 0 paths mismatch: %v", got)
	}
	if got := rules[0].GetService(); got != "projects/app-stage/regions/r/networkEndpointGroups/api-neg" {
		t.Fatalf("rule 0 service mismatch: %q", got)
	}
	if got := rules[1].GetPaths(); len(got) != 1 || got[0] != "/admin/*" {
		t.Fatalf("rule 1 paths mismatch: %v", got)
	}
	if got := rules[1].GetService(); got != "projects/app-stage/regions/r/networkEndpointGroups/admin-neg" {
		t.Fatalf("rule 1 service mismatch: %q", got)
	}
}

// TestUpsertEntry_removesPathRulesOnUpdate proves that upserting the same
// entry without path rules clears any that were present — the matcher is
// replaced wholesale by replacePathMatcher, so no explicit clear is
// needed.
func TestUpsertEntry_removesPathRulesOnUpdate(t *testing.T) {
	base := upsertEntry(fixtureUrlMap(), entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
		PathRules: []pathRuleSpec{
			{Paths: []string{"/api/*"}, Service: "projects/app-stage/regions/r/networkEndpointGroups/api-neg"},
		},
	})
	if matcher := findMatcher(base, "app-stage"); matcher == nil || len(matcher.GetPathRules()) != 1 {
		t.Fatalf("setup: expected one path rule before update")
	}

	out := upsertEntry(base, entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
	})

	matcher := findMatcher(out, "app-stage")
	if matcher == nil {
		t.Fatal("path matcher missing after update")
	}
	if got := matcher.GetPathRules(); len(got) != 0 {
		t.Fatalf("expected path rules cleared, got %d", len(got))
	}
}

// TestFindEntry_extractsPathRules seeds a matcher whose rule service is
// the self-link form GCP returns and asserts findEntry extracts the
// rules and canonicalises each service exactly like default_service.
func TestFindEntry_extractsPathRules(t *testing.T) {
	m := &computepb.UrlMap{
		HostRules: []*computepb.HostRule{
			{Hosts: []string{"app.example.com"}, PathMatcher: proto.String("app")},
		},
		PathMatchers: []*computepb.PathMatcher{
			{
				Name:           proto.String("app"),
				DefaultService: proto.String("projects/p/global/backendServices/b"),
				PathRules: []*computepb.PathRule{
					{
						Paths:   []string{"/api", "/api/*"},
						Service: proto.String("https://www.googleapis.com/compute/v1/projects/p/regions/r/networkEndpointGroups/api-neg"),
					},
				},
			},
		},
	}

	got, ok := findEntry(m, "app")
	if !ok {
		t.Fatal("entry not found")
	}
	if len(got.PathRules) != 1 {
		t.Fatalf("expected 1 path rule, got %d", len(got.PathRules))
	}
	if paths := got.PathRules[0].Paths; len(paths) != 2 || paths[0] != "/api" || paths[1] != "/api/*" {
		t.Fatalf("paths mismatch: %v", paths)
	}
	if svc := got.PathRules[0].Service; svc != "projects/p/regions/r/networkEndpointGroups/api-neg" {
		t.Fatalf("path rule service not canonicalised: %q", svc)
	}
}

// TestUpsertEntry_pathRulesRoundTrip splices an entry with path rules and
// reads it back through findEntry, asserting the spec survives intact.
func TestUpsertEntry_pathRulesRoundTrip(t *testing.T) {
	in := entrySpec{
		Name:           "app-stage",
		Hosts:          []string{"app-stage.gcp.cru.org"},
		DefaultService: "projects/app-stage/regions/r/networkEndpointGroups/serverless-neg",
		PathRules: []pathRuleSpec{
			{Paths: []string{"/api/*"}, Service: "projects/app-stage/regions/r/networkEndpointGroups/api-neg"},
			{Paths: []string{"/admin", "/admin/*"}, Service: "projects/app-stage/regions/r/networkEndpointGroups/admin-neg"},
		},
	}
	out := upsertEntry(fixtureUrlMap(), in)

	got, ok := findEntry(out, "app-stage")
	if !ok {
		t.Fatal("entry not found after upsert")
	}
	if len(got.PathRules) != len(in.PathRules) {
		t.Fatalf("path rule count mismatch: got %d want %d", len(got.PathRules), len(in.PathRules))
	}
	for i := range in.PathRules {
		if got.PathRules[i].Service != in.PathRules[i].Service {
			t.Fatalf("rule %d service mismatch: got %q want %q", i, got.PathRules[i].Service, in.PathRules[i].Service)
		}
		if len(got.PathRules[i].Paths) != len(in.PathRules[i].Paths) {
			t.Fatalf("rule %d path count mismatch: %v vs %v", i, got.PathRules[i].Paths, in.PathRules[i].Paths)
		}
		for j := range in.PathRules[i].Paths {
			if got.PathRules[i].Paths[j] != in.PathRules[i].Paths[j] {
				t.Fatalf("rule %d path %d mismatch: got %q want %q", i, j, got.PathRules[i].Paths[j], in.PathRules[i].Paths[j])
			}
		}
	}
}

func TestFindEntry_halfSplicedReturnsFalse(t *testing.T) {
	m := &computepb.UrlMap{
		HostRules: []*computepb.HostRule{
			{Hosts: []string{"orphan.example.com"}, PathMatcher: proto.String("orphan")},
		},
		// No matching PathMatcher — treat as absent so Read flags
		// the resource for re-creation.
	}
	if _, ok := findEntry(m, "orphan"); ok {
		t.Fatalf("expected findEntry to reject half-spliced state")
	}

	m = &computepb.UrlMap{
		PathMatchers: []*computepb.PathMatcher{
			{Name: proto.String("orphan"), DefaultService: proto.String("x")},
		},
	}
	if _, ok := findEntry(m, "orphan"); ok {
		t.Fatalf("expected findEntry to reject half-spliced state (matcher only)")
	}
}
