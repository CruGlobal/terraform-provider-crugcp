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
