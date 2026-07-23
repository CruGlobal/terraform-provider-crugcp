// Package provider implements the crugcp Terraform provider.
//
// Splicing the per-entry state into a shared *computepb.UrlMap is the
// core of every CRUD operation, so the splice helpers live in their own
// file and are written as pure functions over the in-memory proto. The
// resource layer is responsible for the GET/PATCH round-trip; the
// helpers here only see Go values and never touch the network — which
// keeps the unit tests entirely offline.
package provider

import (
	"sort"
	"strings"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// computeAPIPrefixes are the two self-link forms the Compute API
// returns for global resources. We rewrite either to the canonical
// resource-path form so user-supplied short paths don't perma-diff
// against the long forms the API echoes back on Read.
//
// Anything not matching one of these prefixes is returned untouched —
// over-canonicalising (e.g., rewriting a self link from a different
// API, or a future Compute prefix we don't recognise yet) is a worse
// failure mode than a perma-diff, because state silently disagrees
// with the live resource.
var computeAPIPrefixes = []string{
	"https://www.googleapis.com/compute/v1/",
	"https://compute.googleapis.com/compute/v1/",
}

// canonicalResourcePath strips a Compute API self-link prefix from s
// and returns the trailing resource-path form. Inputs that don't
// match a known prefix are returned unchanged.
func canonicalResourcePath(s string) string {
	for _, p := range computeAPIPrefixes {
		if strings.HasPrefix(s, p) {
			return strings.TrimPrefix(s, p)
		}
	}
	return s
}

// pathRuleSpec mirrors one computepb.PathRule owned by the entry's
// path matcher: requests matching any of Paths route to Service.
type pathRuleSpec struct {
	Paths   []string
	Service string
}

// headerMatchSpec mirrors the supported subset of
// computepb.HttpHeaderMatch: exactly one of Regex ("" = unset) or
// Present (nil = unset) applies to the named header.
type headerMatchSpec struct {
	Name    string
	Regex   string
	Present *bool
}

// queryParamMatchSpec mirrors the supported subset of
// computepb.HttpQueryParameterMatch: exactly one of Exact ("" = unset)
// or Present (nil = unset) applies to the named query parameter.
type queryParamMatchSpec struct {
	Name    string
	Exact   string
	Present *bool
}

// routeMatchSpec mirrors the supported subset of
// computepb.HttpRouteRuleMatch. Exactly one of Prefix or FullPath is
// set ("" = unset); Headers and QueryParams are AND-ed with the path
// condition.
type routeMatchSpec struct {
	Prefix      string
	FullPath    string
	Headers     []headerMatchSpec
	QueryParams []queryParamMatchSpec
}

// redirectSpec mirrors the supported subset of
// computepb.HttpRedirectAction: a path redirect with an explicit
// response code and strip-query behaviour.
type redirectSpec struct {
	Path         string
	ResponseCode string
	StripQuery   bool
}

// routeRuleSpec mirrors the supported subset of
// computepb.HttpRouteRule: requests matching any of Matches either
// route to Service or get Redirect (exactly one is set; Service "" =
// unset). Lower Priority wins.
type routeRuleSpec struct {
	Priority int32
	Matches  []routeMatchSpec
	Service  string
	Redirect *redirectSpec
}

// entrySpec is the in-memory representation of a single host-rule /
// path-matcher pair that this provider owns. One entry maps to exactly
// one HostRule (Hosts → name) plus one PathMatcher (name) on the
// shared URL map. PathRules and RouteRules are mutually exclusive (a
// GCP pathMatcher accepts one or the other, never both); the resource
// layer validates this before the spec is built.
type entrySpec struct {
	Name           string
	Hosts          []string
	DefaultService string
	Description    string
	PathRules      []pathRuleSpec
	RouteRules     []routeRuleSpec
}

// upsertEntry returns a copy of m with the entry's HostRule and
// PathMatcher inserted or replaced. The caller passes the proto fetched
// from Get; the returned proto is what should go in the PATCH body.
// The original is not mutated, so callers don't have to worry about
// retry loops accidentally observing partial edits.
func upsertEntry(m *computepb.UrlMap, e entrySpec) *computepb.UrlMap {
	// proto.Clone preserves the concrete type of its argument, so the
	// assertion is infallible — but the linter can't prove that, and
	// the comma-ok form documents the invariant cheaply.
	out, _ := proto.Clone(m).(*computepb.UrlMap)

	hostRule := &computepb.HostRule{
		Hosts:       append([]string(nil), e.Hosts...),
		PathMatcher: proto.String(e.Name),
	}
	if e.Description != "" {
		hostRule.Description = proto.String(e.Description)
	}

	pathMatcher := &computepb.PathMatcher{
		Name: proto.String(e.Name),
	}
	// DefaultService is optional when route rules cover all traffic
	// themselves; writing an empty string would be rejected by the API.
	if e.DefaultService != "" {
		pathMatcher.DefaultService = proto.String(e.DefaultService)
	}
	if e.Description != "" {
		pathMatcher.Description = proto.String(e.Description)
	}

	if len(e.PathRules) > 0 {
		rules := make([]*computepb.PathRule, 0, len(e.PathRules))
		for _, r := range e.PathRules {
			rules = append(rules, &computepb.PathRule{
				Paths:   append([]string(nil), r.Paths...),
				Service: proto.String(r.Service),
			})
		}
		pathMatcher.PathRules = rules
	}

	if len(e.RouteRules) > 0 {
		// Splice in ascending priority order so the PATCH body is
		// deterministic regardless of how the caller (a Terraform
		// set) ordered the slice.
		rules := append([]routeRuleSpec(nil), e.RouteRules...)
		sort.Slice(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
		pbRules := make([]*computepb.HttpRouteRule, 0, len(rules))
		for _, r := range rules {
			pbRules = append(pbRules, routeRuleToProto(r))
		}
		pathMatcher.RouteRules = pbRules
	}

	out.HostRules = replaceHostRule(out.HostRules, e.Name, hostRule)
	out.PathMatchers = replacePathMatcher(out.PathMatchers, e.Name, pathMatcher)
	return out
}

// removeEntry returns a copy of m with any HostRule pointing at the
// named PathMatcher, plus the PathMatcher itself, removed. Missing
// entries are a no-op — the caller's intent (deletion) is satisfied
// either way.
func removeEntry(m *computepb.UrlMap, name string) *computepb.UrlMap {
	// proto.Clone preserves the concrete type of its argument, so the
	// assertion is infallible — but the linter can't prove that, and
	// the comma-ok form documents the invariant cheaply.
	out, _ := proto.Clone(m).(*computepb.UrlMap)
	out.HostRules = filterHostRules(out.HostRules, name)
	out.PathMatchers = filterPathMatchers(out.PathMatchers, name)
	return out
}

// findEntry locates the HostRule + PathMatcher pair that this resource
// owns. Both must be present and the PathMatcher reference must match;
// a half-spliced state (which can happen if a previous PATCH partially
// applied or if a user hand-edited the URL map) is reported as "not
// found" so Read can mark the resource for re-creation.
func findEntry(m *computepb.UrlMap, name string) (entrySpec, bool) {
	if m == nil {
		return entrySpec{}, false
	}

	var host *computepb.HostRule
	for _, h := range m.HostRules {
		if h.GetPathMatcher() == name {
			host = h
			break
		}
	}
	if host == nil {
		return entrySpec{}, false
	}

	var matcher *computepb.PathMatcher
	for _, p := range m.PathMatchers {
		if p.GetName() == name {
			matcher = p
			break
		}
	}
	if matcher == nil {
		return entrySpec{}, false
	}

	// Extract path rules, canonicalising each service exactly like
	// DefaultService — the API echoes self links here too.
	var rules []pathRuleSpec
	for _, r := range matcher.GetPathRules() {
		rules = append(rules, pathRuleSpec{
			Paths:   append([]string(nil), r.GetPaths()...),
			Service: canonicalResourcePath(r.GetService()),
		})
	}

	var routeRules []routeRuleSpec
	for _, r := range matcher.GetRouteRules() {
		routeRules = append(routeRules, routeRuleFromProto(r))
	}

	return entrySpec{
		Name:  name,
		Hosts: append([]string(nil), host.GetHosts()...),
		// Canonicalise the default-service path so callers using the
		// documented short form don't perma-diff against the self
		// link the API echoes back. findEntry is the single point
		// where API-returned values enter our types, so handling it
		// here covers Read, post-Patch re-read, and Import.
		DefaultService: canonicalResourcePath(matcher.GetDefaultService()),
		// Prefer the HostRule's description, but fall back to the
		// PathMatcher's: this provider writes the same value to both,
		// so either being present is enough. Surface neither field
		// being set as an empty string (== absent in state).
		Description: firstNonEmpty(host.GetDescription(), matcher.GetDescription()),
		PathRules:   rules,
		RouteRules:  routeRules,
	}, true
}

// routeRuleToProto lowers the supported routeRuleSpec subset into the
// wire proto. Unset optional fields are left nil so the API never sees
// empty strings where "absent" is meant.
func routeRuleToProto(r routeRuleSpec) *computepb.HttpRouteRule {
	out := &computepb.HttpRouteRule{
		Priority: proto.Int32(r.Priority),
	}
	for _, m := range r.Matches {
		pm := &computepb.HttpRouteRuleMatch{}
		if m.Prefix != "" {
			pm.PrefixMatch = proto.String(m.Prefix)
		}
		if m.FullPath != "" {
			pm.FullPathMatch = proto.String(m.FullPath)
		}
		for _, h := range m.Headers {
			hm := &computepb.HttpHeaderMatch{HeaderName: proto.String(h.Name)}
			if h.Regex != "" {
				hm.RegexMatch = proto.String(h.Regex)
			}
			if h.Present != nil {
				hm.PresentMatch = proto.Bool(*h.Present)
			}
			pm.HeaderMatches = append(pm.HeaderMatches, hm)
		}
		for _, q := range m.QueryParams {
			qm := &computepb.HttpQueryParameterMatch{Name: proto.String(q.Name)}
			if q.Exact != "" {
				qm.ExactMatch = proto.String(q.Exact)
			}
			if q.Present != nil {
				qm.PresentMatch = proto.Bool(*q.Present)
			}
			pm.QueryParameterMatches = append(pm.QueryParameterMatches, qm)
		}
		out.MatchRules = append(out.MatchRules, pm)
	}
	if r.Service != "" {
		out.Service = proto.String(r.Service)
	}
	if r.Redirect != nil {
		out.UrlRedirect = &computepb.HttpRedirectAction{
			PathRedirect:         proto.String(r.Redirect.Path),
			RedirectResponseCode: proto.String(r.Redirect.ResponseCode),
			StripQuery:           proto.Bool(r.Redirect.StripQuery),
		}
	}
	return out
}

// routeRuleFromProto is the inverse of routeRuleToProto for the fields
// this provider supports; anything outside the subset is dropped, which
// Read surfaces as a diff rather than silently keeping it. Services are
// canonicalised exactly like DefaultService.
func routeRuleFromProto(r *computepb.HttpRouteRule) routeRuleSpec {
	spec := routeRuleSpec{
		Priority: r.GetPriority(),
		Service:  canonicalResourcePath(r.GetService()),
	}
	for _, m := range r.GetMatchRules() {
		ms := routeMatchSpec{
			Prefix:   m.GetPrefixMatch(),
			FullPath: m.GetFullPathMatch(),
		}
		for _, h := range m.GetHeaderMatches() {
			hs := headerMatchSpec{
				Name:  h.GetHeaderName(),
				Regex: h.GetRegexMatch(),
			}
			if h.PresentMatch != nil {
				v := h.GetPresentMatch()
				hs.Present = &v
			}
			ms.Headers = append(ms.Headers, hs)
		}
		for _, q := range m.GetQueryParameterMatches() {
			qs := queryParamMatchSpec{
				Name:  q.GetName(),
				Exact: q.GetExactMatch(),
			}
			if q.PresentMatch != nil {
				v := q.GetPresentMatch()
				qs.Present = &v
			}
			ms.QueryParams = append(ms.QueryParams, qs)
		}
		spec.Matches = append(spec.Matches, ms)
	}
	if rd := r.GetUrlRedirect(); rd != nil {
		spec.Redirect = &redirectSpec{
			Path:         rd.GetPathRedirect(),
			ResponseCode: rd.GetRedirectResponseCode(),
			StripQuery:   rd.GetStripQuery(),
		}
	}
	return spec
}

// replaceHostRule rewrites the slice with the new value if a HostRule
// with the matching path_matcher already exists; otherwise it appends.
// Order is preserved for upserts so plan diffs stay quiet.
func replaceHostRule(in []*computepb.HostRule, name string, replacement *computepb.HostRule) []*computepb.HostRule {
	for i, h := range in {
		if h.GetPathMatcher() == name {
			out := make([]*computepb.HostRule, len(in))
			copy(out, in)
			out[i] = replacement
			return out
		}
	}
	return append(append([]*computepb.HostRule(nil), in...), replacement)
}

// replacePathMatcher mirrors replaceHostRule for path matchers.
func replacePathMatcher(in []*computepb.PathMatcher, name string, replacement *computepb.PathMatcher) []*computepb.PathMatcher {
	for i, p := range in {
		if p.GetName() == name {
			out := make([]*computepb.PathMatcher, len(in))
			copy(out, in)
			out[i] = replacement
			return out
		}
	}
	return append(append([]*computepb.PathMatcher(nil), in...), replacement)
}

func filterHostRules(in []*computepb.HostRule, name string) []*computepb.HostRule {
	out := make([]*computepb.HostRule, 0, len(in))
	for _, h := range in {
		if h.GetPathMatcher() == name {
			continue
		}
		out = append(out, h)
	}
	return out
}

func filterPathMatchers(in []*computepb.PathMatcher, name string) []*computepb.PathMatcher {
	out := make([]*computepb.PathMatcher, 0, len(in))
	for _, p := range in {
		if p.GetName() == name {
			continue
		}
		out = append(out, p)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
