package provider

import (
	"reflect"
	"testing"
)

const iapRedirect = "https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect"

func TestHasClientIDPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		uris []string
		want bool
	}{
		{name: "nil", uris: nil, want: false},
		{name: "no placeholder", uris: []string{"https://example.test/cb"}, want: false},
		{name: "iap redirect", uris: []string{iapRedirect}, want: true},
		{name: "second entry only", uris: []string{"https://example.test/cb", iapRedirect}, want: true},
		// Near-misses are not the placeholder; the schema validator
		// rejects them, but the substituter must not guess either.
		{name: "near miss", uris: []string{"https://example.test/{client_id}"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasClientIDPlaceholder(tc.uris); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSubstituteClientID_doesNotMutateInput(t *testing.T) {
	in := []string{iapRedirect}
	out := substituteClientID(in, "abc-123")

	want := "https://iap.googleapis.com/v1/oauth/clientIds/abc-123:handleRedirect"
	if out[0] != want {
		t.Fatalf("substituted = %q, want %q", out[0], want)
	}
	// The config-shaped value has to survive: it's what gets written
	// back to state so `{clientid}` shows up in the plan, not a UUID.
	if in[0] != iapRedirect {
		t.Fatalf("input was mutated: %q", in[0])
	}
}

// TestSubstituteRoundTrip is the invariant the resource leans on when
// deciding whether the API has drifted: resolving config and then
// folding the id back must return the original.
func TestSubstituteRoundTrip(t *testing.T) {
	const clientID = "287fcecb-aca5-4a51-b9b7-a62149b3adaf"
	in := []string{iapRedirect, "https://example.test/callback"}

	resolved := substituteClientID(in, clientID)
	back := unsubstituteClientID(resolved, clientID)

	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip lost fidelity:\n got %#v\nwant %#v", back, in)
	}
}

func TestUnsubstituteClientID_emptyClientIDIsPassthrough(t *testing.T) {
	// An empty client id must not turn every URI into one giant
	// placeholder — strings.ReplaceAll with an empty needle would.
	in := []string{"https://example.test/cb"}
	got := unsubstituteClientID(in, "")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %#v want %#v", got, in)
	}
}

func TestSameStringMultiset(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{name: "identical", a: []string{"a", "b"}, b: []string{"a", "b"}, want: true},
		// The IAM API doesn't promise to echo the list back in order;
		// an order-sensitive compare would report permanent drift.
		{name: "reordered", a: []string{"a", "b"}, b: []string{"b", "a"}, want: true},
		{name: "different length", a: []string{"a"}, b: []string{"a", "b"}, want: false},
		{name: "different content", a: []string{"a", "b"}, b: []string{"a", "c"}, want: false},
		{name: "duplicates matter", a: []string{"a", "a"}, b: []string{"a", "b"}, want: false},
		{name: "both empty", a: nil, b: nil, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameStringMultiset(tc.a, tc.b); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSameStringMultiset_doesNotReorderInputs(t *testing.T) {
	a := []string{"b", "a"}
	sameStringMultiset(a, []string{"a", "b"})
	if a[0] != "b" {
		t.Fatalf("input slice was sorted in place: %#v", a)
	}
}

func TestUnknownPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want []string
	}{
		{name: "supported placeholder", uri: iapRedirect, want: nil},
		{name: "no braces", uri: "https://example.test/cb", want: nil},
		{name: "underscore typo", uri: "https://example.test/{client_id}", want: []string{"{client_id}"}},
		{name: "camel typo", uri: "https://example.test/{clientId}", want: []string{"{clientId}"}},
		{
			name: "mixed",
			uri:  "https://example.test/{clientid}/{env}",
			want: []string{"{env}"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unknownPlaceholders(tc.uri)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestProvisionalClientIDIsURISafe(t *testing.T) {
	// The provisional value stands in on the initial POST, where the
	// API validates allowedRedirectUris. A bare "{clientid}" is not a
	// legal URI, which is the whole reason this constant exists.
	got := substituteClientID([]string{iapRedirect}, provisionalClientID)[0]
	want := "https://iap.googleapis.com/v1/oauth/clientIds/00000000-0000-0000-0000-000000000000:handleRedirect"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
