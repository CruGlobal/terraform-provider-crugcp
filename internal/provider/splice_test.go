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
