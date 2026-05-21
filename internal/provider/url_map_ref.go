package provider

import (
	"fmt"
	"strings"
)

// urlMapRef identifies a single global Compute URL map. The provider accepts
// either the resource-path form (projects/{project}/global/urlMaps/{name}) or
// the full self link (https://www.googleapis.com/compute/v1/...), and
// normalizes to the resource-path form internally.
type urlMapRef struct {
	Project string
	Name    string
}

// String renders the canonical resource path. Used as the stable identity
// portion of the resource ID and for state.
func (r urlMapRef) String() string {
	return fmt.Sprintf("projects/%s/global/urlMaps/%s", r.Project, r.Name)
}

// parseURLMapRef accepts the inputs documented above. Regional URL maps
// (urlMaps under /regions/{region}/) are not supported — the shared LB use
// case is always a Global External ALB, and reading the regional resource
// requires a different API client.
func parseURLMapRef(s string) (urlMapRef, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return urlMapRef{}, fmt.Errorf("url_map is empty")
	}

	// Strip a leading scheme + host so the same parser handles both the
	// short and self-link forms.
	if i := strings.Index(trimmed, "/compute/v1/"); i >= 0 {
		trimmed = trimmed[i+len("/compute/v1/"):]
	}
	trimmed = strings.TrimPrefix(trimmed, "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) != 5 ||
		parts[0] != "projects" || parts[1] == "" ||
		parts[2] != "global" ||
		parts[3] != "urlMaps" || parts[4] == "" {
		return urlMapRef{}, fmt.Errorf("url_map %q is not in the form projects/{project}/global/urlMaps/{name}", s)
	}
	return urlMapRef{Project: parts[1], Name: parts[4]}, nil
}
