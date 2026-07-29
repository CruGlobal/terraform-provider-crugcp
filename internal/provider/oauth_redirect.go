package provider

import (
	"regexp"
	"sort"
	"strings"
)

// clientIDPlaceholder is the token users write inside
// allowed_redirect_uris where the OAuth client's server-generated
// client id belongs. The IAM API only mints that id during Create, so
// a redirect URI that has to embed it cannot be expressed in a single
// declarative POST — hence the placeholder plus a self-PATCH.
//
// The spelling matches the one used in the upstream feature request
// (hashicorp/terraform-provider-google#22530), so configs written
// against a future google-provider implementation of the same idea
// should port across unchanged.
const clientIDPlaceholder = "{clientid}"

// provisionalClientID stands in for the real client id on the initial
// POST. allowed_redirect_uris is a required field, so something has to
// go in it before the id exists; a nil UUID keeps the value a
// syntactically valid URI (which a bare "{clientid}" is not) and is
// obviously-fake if an apply dies between the POST and the PATCH.
const provisionalClientID = "00000000-0000-0000-0000-000000000000"

// placeholderToken matches any {braced} token so the schema validator
// can reject near-misses like {client_id} or {clientId}. Silently
// posting an unrecognised token would produce a redirect URI that
// looks right in state and fails at sign-in time.
var placeholderToken = regexp.MustCompile(`\{[^}]*\}`)

// unknownPlaceholders returns the {braced} tokens in uri that are not
// the supported placeholder, in order of appearance.
func unknownPlaceholders(uri string) []string {
	var unknown []string
	for _, tok := range placeholderToken.FindAllString(uri, -1) {
		if tok != clientIDPlaceholder {
			unknown = append(unknown, tok)
		}
	}
	return unknown
}

// hasClientIDPlaceholder reports whether any URI needs substitution —
// i.e. whether Create has to do the POST-then-PATCH dance at all.
func hasClientIDPlaceholder(uris []string) bool {
	for _, u := range uris {
		if strings.Contains(u, clientIDPlaceholder) {
			return true
		}
	}
	return false
}

// substituteClientID renders the configured URIs into the form that is
// actually sent to the API. Returns a fresh slice; the input is left
// alone so the config-shaped values can still be written to state.
func substituteClientID(uris []string, clientID string) []string {
	out := make([]string, len(uris))
	for i, u := range uris {
		out[i] = strings.ReplaceAll(u, clientIDPlaceholder, clientID)
	}
	return out
}

// unsubstituteClientID is the inverse: it turns API-shaped URIs back
// into config-shaped ones by folding the literal client id back into
// the placeholder. Used when the API's list has drifted away from what
// the config asked for, so the resulting plan diff reads in the same
// vocabulary the user wrote.
//
// An empty clientID (nothing to fold) passes the list through.
func unsubstituteClientID(uris []string, clientID string) []string {
	out := make([]string, len(uris))
	copy(out, uris)
	if clientID == "" {
		return out
	}
	for i, u := range out {
		out[i] = strings.ReplaceAll(u, clientID, clientIDPlaceholder)
	}
	return out
}

// sameStringMultiset compares two URI lists ignoring order. The IAM
// API does not promise to echo allowedRedirectUris back in the order
// it was sent, so an order-sensitive comparison would report drift on
// a perfectly in-sync client and reshuffle state on every read.
func sameStringMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
