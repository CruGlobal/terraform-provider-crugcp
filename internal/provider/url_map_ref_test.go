package provider

import "testing"

func TestParseURLMapRef(t *testing.T) {
	cases := []struct {
		in        string
		wantProj  string
		wantName  string
		wantError bool
	}{
		{
			in:       "projects/cru-shared-cloudrun-lb/global/urlMaps/internal-shared",
			wantProj: "cru-shared-cloudrun-lb",
			wantName: "internal-shared",
		},
		{
			in:       "https://www.googleapis.com/compute/v1/projects/p/global/urlMaps/m",
			wantProj: "p",
			wantName: "m",
		},
		{
			in:       "//compute.googleapis.com/compute/v1/projects/p/global/urlMaps/m",
			wantProj: "p",
			wantName: "m",
		},
		{in: "", wantError: true},
		{in: "projects//global/urlMaps/m", wantError: true},
		{in: "projects/p/regions/us-central1/urlMaps/m", wantError: true},
		{in: "projects/p/global/backendServices/m", wantError: true},
		{in: "projects/p/global/urlMaps/", wantError: true},
		{in: "p/m", wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseURLMapRef(tc.in)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got.Project != tc.wantProj || got.Name != tc.wantName {
				t.Fatalf("got %+v, want project=%s name=%s", got, tc.wantProj, tc.wantName)
			}
		})
	}
}

func TestURLMapRefString(t *testing.T) {
	r := urlMapRef{Project: "p", Name: "m"}
	if got := r.String(); got != "projects/p/global/urlMaps/m" {
		t.Fatalf("unexpected canonical form: %s", got)
	}
}
