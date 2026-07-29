package provider

import (
	"fmt"
	"strings"
)

// oauthClientRef identifies a single IAM OAuth client. The IAM API
// addresses these by the resource name
// projects/{project}/locations/{location}/oauthClients/{oauth_client_id};
// the ref keeps the three components apart so they can be surfaced as
// individual attributes and reassembled for API calls.
type oauthClientRef struct {
	Project  string
	Location string
	ClientID string
}

// String renders the canonical resource name. This doubles as the
// Terraform resource ID and the import ID.
func (r oauthClientRef) String() string {
	return fmt.Sprintf("projects/%s/locations/%s/oauthClients/%s", r.Project, r.Location, r.ClientID)
}

// Parent renders the collection the client lives in, which is what
// Create takes.
func (r oauthClientRef) Parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", r.Project, r.Location)
}

// parseOAuthClientRef accepts the canonical resource name, optionally
// prefixed with the API host (as returned in some response payloads)
// or a leading slash.
func parseOAuthClientRef(s string) (oauthClientRef, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return oauthClientRef{}, fmt.Errorf("oauth client name is empty")
	}

	if i := strings.Index(trimmed, "/v1/"); i >= 0 {
		trimmed = trimmed[i+len("/v1/"):]
	}
	trimmed = strings.TrimPrefix(trimmed, "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) != 6 ||
		parts[0] != "projects" || parts[1] == "" ||
		parts[2] != "locations" || parts[3] == "" ||
		parts[4] != "oauthClients" || parts[5] == "" {
		return oauthClientRef{}, fmt.Errorf(
			"%q is not in the form projects/{project}/locations/{location}/oauthClients/{oauth_client_id}", s)
	}
	return oauthClientRef{Project: parts[1], Location: parts[3], ClientID: parts[5]}, nil
}
