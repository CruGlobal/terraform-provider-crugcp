package provider

import (
	"errors"
	"strings"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// urlMapWithEntries builds a URL map holding one entry per name.
func urlMapWithEntries(names ...string) *computepb.UrlMap {
	m := &computepb.UrlMap{
		Name:           proto.String("shared"),
		Fingerprint:    proto.String("fp0"),
		DefaultService: proto.String("projects/p/global/backendServices/b"),
	}
	for _, n := range names {
		m.HostRules = append(m.HostRules, &computepb.HostRule{
			Hosts:       []string{n + ".example.test"},
			PathMatcher: proto.String(n),
		})
		m.PathMatchers = append(m.PathMatchers, &computepb.PathMatcher{
			Name:           proto.String(n),
			DefaultService: proto.String("projects/p/global/backendServices/b"),
		})
	}
	return m
}

func TestNeedsFullUpdate(t *testing.T) {
	cases := []struct {
		name string
		in   *computepb.UrlMap
		want bool
	}{
		{
			// The bug: deleting the only entry empties both lists, and
			// a merge PATCH cannot express that.
			name: "removing the last entry needs PUT",
			in:   removeEntry(urlMapWithEntries("only"), "only"),
			want: true,
		},
		{
			// One entry survives, so both lists are non-empty, present
			// in the body, and replaced wholesale by the merge patch.
			name: "removing one of two stays on PATCH",
			in:   removeEntry(urlMapWithEntries("a", "b"), "a"),
			want: false,
		},
		{
			name: "adding an entry to an empty map stays on PATCH",
			in:   upsertEntry(urlMapWithEntries(), entrySpec{Name: "new", Hosts: []string{"h"}, DefaultService: "svc"}),
			want: false,
		},
		{
			name: "unchanged populated map stays on PATCH",
			in:   urlMapWithEntries("a"),
			want: false,
		},
		{name: "nil is not a clearing write", in: nil, want: false},
		{
			// Defensive: the provider keeps the two lists in lockstep,
			// but either one being empty is unexpressible via PATCH.
			name: "host rules empty alone still needs PUT",
			in: &computepb.UrlMap{
				PathMatchers: urlMapWithEntries("a").PathMatchers,
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsFullUpdate(tc.in); got != tc.want {
				t.Fatalf("needsFullUpdate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsResourceNotReady pins the second flake fixed alongside the
// merge-patch bug: Compute answers a write with HTTP 400 "is not ready"
// while an earlier operation on the same URL map settles, and that has
// to be retried rather than failing the apply.
func TestIsResourceNotReady(t *testing.T) {
	notReady := func() error {
		return status.Error(codes.InvalidArgument,
			"googleapi: Error 400: The resource 'projects/p/global/urlMaps/shared' is not ready, resourceNotReady")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "not ready", err: notReady(), want: true},
		{
			// A genuinely invalid spec must fail fast, not be retried
			// for 25 seconds — hence matching the message, not just 400.
			name: "other invalid argument",
			err:  status.Error(codes.InvalidArgument, "googleapi: Error 400: Invalid value for field 'hostRules[0].hosts'"),
			want: false,
		},
		{
			name: "not found is not retryable here",
			err:  status.Error(codes.NotFound, "googleapi: Error 404: not found"),
			want: false,
		},
		{name: "plain error", err: errors.New("boom"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResourceNotReady(tc.err); got != tc.want {
				t.Fatalf("isResourceNotReady = %v, want %v", got, tc.want)
			}
		})
	}

	// The loop's predicate must accept both retryable conditions.
	if !isRetryableWriteError(notReady()) {
		t.Error("isRetryableWriteError should accept a not-ready error")
	}
	if !isRetryableWriteError(status.Error(codes.FailedPrecondition, "fingerprint mismatch")) {
		t.Error("isRetryableWriteError should still accept a fingerprint conflict")
	}
	if isRetryableWriteError(errors.New("boom")) {
		t.Error("isRetryableWriteError should reject unrelated errors")
	}
}

// TestEmptyListsVanishFromPatchBody is a canary for the SDK behaviour
// that forces needsFullUpdate to exist. It marshals with the same
// options the generated Compute client hard-codes
// (protojson.MarshalOptions{AllowPartial: true}) and asserts that empty
// repeated fields disappear from the request body entirely.
//
// If this test ever FAILS because the keys are now emitted as [], that
// is good news: a merge PATCH would then be able to clear the lists,
// and needsFullUpdate plus the Update/PUT branch in applyEntry can be
// deleted. Don't just fix the assertion — check whether the workaround
// is still needed.
func TestEmptyListsVanishFromPatchBody(t *testing.T) {
	emptied := removeEntry(urlMapWithEntries("only"), "only")

	if len(emptied.GetHostRules()) != 0 || len(emptied.GetPathMatchers()) != 0 {
		t.Fatalf("precondition: expected both lists emptied, got %d host rules and %d path matchers",
			len(emptied.GetHostRules()), len(emptied.GetPathMatchers()))
	}

	body, err := protojson.MarshalOptions{AllowPartial: true}.Marshal(emptied)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}

	for _, key := range []string{"hostRules", "pathMatchers"} {
		if strings.Contains(string(body), key) {
			t.Errorf("%q is present in the marshalled body (%s) — empty lists are now emitted, so the "+
				"Update/PUT workaround in applyEntry may no longer be necessary; re-check needsFullUpdate",
				key, body)
		}
	}

	// Sanity check the other half of the contract: a non-empty list is
	// present, which is why a merge PATCH replaces it correctly.
	populated, err := protojson.MarshalOptions{AllowPartial: true}.Marshal(urlMapWithEntries("a"))
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	for _, key := range []string{"hostRules", "pathMatchers"} {
		if !strings.Contains(string(populated), key) {
			t.Errorf("%q missing from a populated body (%s)", key, populated)
		}
	}
}
