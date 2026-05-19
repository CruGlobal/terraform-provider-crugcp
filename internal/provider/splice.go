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
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// entrySpec is the in-memory representation of a single host-rule /
// path-matcher pair that this provider owns. One entry maps to exactly
// one HostRule (Hosts → name) plus one PathMatcher (name) on the
// shared URL map.
type entrySpec struct {
	Name           string
	Hosts          []string
	DefaultService string
	Description    string
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
		Name:           proto.String(e.Name),
		DefaultService: proto.String(e.DefaultService),
	}
	if e.Description != "" {
		pathMatcher.Description = proto.String(e.Description)
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

	return entrySpec{
		Name:           name,
		Hosts:          append([]string(nil), host.GetHosts()...),
		DefaultService: matcher.GetDefaultService(),
		// Prefer the HostRule's description, but fall back to the
		// PathMatcher's: this provider writes the same value to both,
		// so either being present is enough. Surface neither field
		// being set as an empty string (== absent in state).
		Description: firstNonEmpty(host.GetDescription(), matcher.GetDescription()),
	}, true
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
