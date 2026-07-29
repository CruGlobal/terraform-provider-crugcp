package provider

import "testing"

func TestParseOAuthClientRef(t *testing.T) {
	want := oauthClientRef{Project: "cru-app-stage", Location: "global", ClientID: "iap-workforce"}

	cases := []struct {
		name    string
		in      string
		want    oauthClientRef
		wantErr bool
	}{
		{
			name: "canonical",
			in:   "projects/cru-app-stage/locations/global/oauthClients/iap-workforce",
			want: want,
		},
		{
			name: "leading slash",
			in:   "/projects/cru-app-stage/locations/global/oauthClients/iap-workforce",
			want: want,
		},
		{
			name: "self link",
			in:   "https://iam.googleapis.com/v1/projects/cru-app-stage/locations/global/oauthClients/iap-workforce",
			want: want,
		},
		{name: "surrounding whitespace", in: "  " + want.String() + "\n", want: want},
		{name: "empty", in: "", wantErr: true},
		{name: "too few segments", in: "projects/p/locations/global", wantErr: true},
		{name: "wrong collection", in: "projects/p/locations/global/oauthClient/x", wantErr: true},
		{name: "missing client id", in: "projects/p/locations/global/oauthClients/", wantErr: true},
		{name: "url map path", in: "projects/p/global/urlMaps/m", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOAuthClientRef(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestOAuthClientRefRendering(t *testing.T) {
	ref := oauthClientRef{Project: "p", Location: "global", ClientID: "c"}

	if got, want := ref.String(), "projects/p/locations/global/oauthClients/c"; got != want {
		t.Fatalf("String() = %q want %q", got, want)
	}
	if got, want := ref.Parent(), "projects/p/locations/global"; got != want {
		t.Fatalf("Parent() = %q want %q", got, want)
	}

	// Round-tripping String() keeps import IDs and state IDs aligned.
	back, err := parseOAuthClientRef(ref.String())
	if err != nil {
		t.Fatalf("round trip failed: %s", err)
	}
	if back != ref {
		t.Fatalf("round trip changed the ref: got %+v want %+v", back, ref)
	}
}
