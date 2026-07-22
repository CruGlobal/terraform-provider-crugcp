package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccURLMapHostRule_basic exercises Create, Read, Update (hosts +
// default_service), and Delete in one apply chain. ImportState is run
// between Create and Update to make sure the round-trip is lossless.
func TestAccURLMapHostRule_basic(t *testing.T) {
	name := fmt.Sprintf("crugcp-acc-%s", strings.ToLower(acctest.RandString(8)))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheck(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckURLMapEntryDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: testAccBasicConfig(name, "default", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "name", name),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "hosts.#", "1"),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "hosts.0", name+".example.test"),
					resource.TestCheckResourceAttrSet("crugcp_compute_url_map_host_rule.test", "default_service"),
					resource.TestCheckResourceAttrSet("crugcp_compute_url_map_host_rule.test", "project"),
					resource.TestCheckResourceAttrSet("crugcp_compute_url_map_host_rule.test", "url_map_name"),
					testAccCheckURLMapEntryExists(name),
				),
			},
			{
				ResourceName:      "crugcp_compute_url_map_host_rule.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: importIDFunc("crugcp_compute_url_map_host_rule.test"),
			},
			{
				// Change both the host list and the default_service
				// to exercise the read-modify-write path.
				Config: testAccBasicConfig(name, "alt", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "hosts.#", "2"),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "hosts.0", name+".example.test"),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "hosts.1", name+"-renamed.example.test"),
					testAccCheckURLMapEntryExists(name),
				),
			},
		},
	})
}

// TestAccURLMapHostRule_twoEntries proves that two crugcp resources
// can target the same URL map concurrently without clobbering each
// other. Both should still be present at the end of the apply.
func TestAccURLMapHostRule_twoEntries(t *testing.T) {
	nameA := fmt.Sprintf("crugcp-acc-a-%s", strings.ToLower(acctest.RandString(6)))
	nameB := fmt.Sprintf("crugcp-acc-b-%s", strings.ToLower(acctest.RandString(6)))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheck(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if err := testAccCheckURLMapEntryDestroyed(nameA)(s); err != nil {
				return err
			}
			return testAccCheckURLMapEntryDestroyed(nameB)(s)
		},
		Steps: []resource.TestStep{
			{
				Config: testAccTwoEntriesConfig(nameA, nameB),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckURLMapEntryExists(nameA),
					testAccCheckURLMapEntryExists(nameB),
				),
			},
			{
				// Drop entry A; B should be untouched.
				Config: testAccOneEntryConfig(nameB),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckURLMapEntryAbsent(nameA),
					testAccCheckURLMapEntryExists(nameB),
				),
			},
		},
	})
}

// TestAccURLMapHostRule_pathRules exercises the optional path_rules
// attribute: Create with one rule, Update the rule's path list, then
// remove path_rules entirely. The rule's service reuses the same backend
// as default_service — routing /api/* to the same backend is valid and
// avoids provisioning a second backend just for the test.
func TestAccURLMapHostRule_pathRules(t *testing.T) {
	name := fmt.Sprintf("crugcp-acc-%s", strings.ToLower(acctest.RandString(8)))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheck(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckURLMapEntryDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: testAccPathRulesConfig(name, []string{"/api/*"}, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "name", name),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "path_rules.#", "1"),
					// path_rules and paths are sets: assert on membership,
					// not index, so a differing API echo order can't fail
					// the test spuriously.
					resource.TestCheckTypeSetElemNestedAttrs("crugcp_compute_url_map_host_rule.test", "path_rules.*", map[string]string{
						"paths.#": "1",
					}),
					// The live URL map is the authoritative check that the
					// path set round-trips regardless of order — the exact
					// concern behind modelling these as sets.
					testAccCheckPathRulePaths(name, "/api/*"),
					testAccCheckURLMapEntryExists(name),
				),
			},
			{
				ResourceName:      "crugcp_compute_url_map_host_rule.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: importIDFunc("crugcp_compute_url_map_host_rule.test"),
			},
			{
				// Extend the path set to exercise the update path.
				Config: testAccPathRulesConfig(name, []string{"/api", "/api/*"}, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "path_rules.#", "1"),
					resource.TestCheckTypeSetElemNestedAttrs("crugcp_compute_url_map_host_rule.test", "path_rules.*", map[string]string{
						"paths.#": "2",
					}),
					testAccCheckPathRulePaths(name, "/api", "/api/*"),
					testAccCheckURLMapEntryExists(name),
				),
			},
			{
				// Drop path_rules entirely; the rules should clear.
				Config: testAccPathRulesConfig(name, nil, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "path_rules.#", "0"),
					testAccCheckURLMapEntryExists(name),
				),
			},
		},
	})
}

// TestAccURLMapHostRule_routeRules exercises route_rules end to end
// with the IAP-signin shape that motivated the feature: prefix
// matches, a query-param present match, a Cookie-header regex match,
// and a catch-all redirect. It then updates the redirect's response
// code and finally swaps back to a plain default_service entry,
// proving the rules clear.
//
// The parent URL map must use load_balancing_scheme EXTERNAL_MANAGED —
// classic Application Load Balancers reject routeRules.
func TestAccURLMapHostRule_routeRules(t *testing.T) {
	name := fmt.Sprintf("crugcp-acc-%s", strings.ToLower(acctest.RandString(8)))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheck(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckURLMapEntryDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: testAccRouteRulesConfig(name, "FOUND"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "name", name),
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "route_rules.#", "4"),
					testAccCheckRouteRules(name, "FOUND"),
					testAccCheckURLMapEntryExists(name),
				),
			},
			{
				ResourceName:      "crugcp_compute_url_map_host_rule.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: importIDFunc("crugcp_compute_url_map_host_rule.test"),
			},
			{
				// Change the redirect's response code to exercise the
				// update path through the route-rule splice.
				Config: testAccRouteRulesConfig(name, "SEE_OTHER"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckRouteRules(name, "SEE_OTHER"),
					testAccCheckURLMapEntryExists(name),
				),
			},
			{
				// Swap back to a plain default_service entry; the route
				// rules should clear.
				Config: testAccBasicConfig(name, "default", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("crugcp_compute_url_map_host_rule.test", "route_rules.#", "0"),
					testAccCheckRouteRulesAbsent(name),
					testAccCheckURLMapEntryExists(name),
				),
			},
		},
	})
}

// Optional envs for the classic-LB error-path test: a URL map whose
// backends use the classic EXTERNAL scheme, plus a backend service on
// it. Classic ALBs reject routeRules; the test proves the API error
// surfaces cleanly. Skipped when unset.
const (
	envClassicURLMap         = "CRUGCP_ACC_CLASSIC_URL_MAP"
	envClassicBackendService = "CRUGCP_ACC_CLASSIC_BACKEND_SERVICE"
)

// TestAccURLMapHostRule_routeRulesClassicRejected asserts the
// documented limitation: applying route_rules against a classic
// (non-EXTERNAL_MANAGED) URL map fails with the API's error rather
// than something confusing. Empirically the API tolerates a plain
// prefix-match route rule on an unattached classic map — it is the
// advanced criteria (the header regex here) that trigger rejection,
// which is also the feature route_rules exists for.
func TestAccURLMapHostRule_routeRulesClassicRejected(t *testing.T) {
	if os.Getenv(envClassicURLMap) == "" || os.Getenv(envClassicBackendService) == "" {
		t.Skipf("%s and %s must be set for the classic error-path test", envClassicURLMap, envClassicBackendService)
	}
	name := fmt.Sprintf("crugcp-acc-%s", strings.ToLower(acctest.RandString(8)))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheck(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "test" {
  url_map         = %[1]q
  name            = %[2]q
  hosts           = [%[3]q]
  default_service = %[4]q

  route_rules = [
    {
      priority = 1
      match = [{
        prefix  = "/"
        headers = [{ name = "Cookie", regex = ".*GCP_IAA?P_AUTH_TOKEN.*" }]
      }]
      service = %[4]q
    },
  ]
}
`,
					os.Getenv(envClassicURLMap),
					name,
					name+".example.test",
					os.Getenv(envClassicBackendService),
				),
				ExpectError: regexp.MustCompile(`(?i)route ?rules`),
			},
		},
	})
}

// testAccRouteRulesConfig renders the signin-pattern entry. Both
// backends reuse envBackendService — the routing decision, not the
// destination, is what's under test.
func testAccRouteRulesConfig(name, redirectCode string) string {
	return fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "test" {
  url_map         = %[1]q
  name            = %[2]q
  hosts           = [%[3]q]
  default_service = %[4]q

  route_rules = [
    {
      priority = 1
      match    = [{ prefix = "/signin" }, { prefix = "/assets/" }]
      service  = %[4]q
    },
    {
      priority = 2
      match = [{
        prefix       = "/"
        query_params = [{ name = "gcp-iap-mode", present = true }]
      }]
      service = %[4]q
    },
    {
      priority = 3
      match = [{
        prefix  = "/"
        headers = [{ name = "Cookie", regex = ".*GCP_IAA?P_AUTH_TOKEN.*" }]
      }]
      service = %[4]q
    },
    {
      priority = 4
      match    = [{ prefix = "/" }]
      redirect = {
        path          = "/signin"
        response_code = %[5]q
        strip_query   = false
      }
    },
  ]
}
`,
		os.Getenv(envURLMap),
		name,
		name+".example.test",
		os.Getenv(envBackendService),
		redirectCode,
	)
}

// testAccCheckRouteRules asserts against the live URL map that the
// four signin-pattern rules survived the apply with their match
// criteria and the expected redirect response code.
func testAccCheckRouteRules(name, redirectCode string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		entry, err := fetchLiveEntry(name)
		if err != nil {
			return err
		}
		if len(entry.RouteRules) != 4 {
			return fmt.Errorf("expected 4 route rules on %q, got %d", name, len(entry.RouteRules))
		}
		byPriority := make(map[int32]routeRuleSpec, len(entry.RouteRules))
		for _, r := range entry.RouteRules {
			byPriority[r.Priority] = r
		}
		if r := byPriority[1]; len(r.Matches) != 2 || r.Service == "" {
			return fmt.Errorf("priority 1 rule mismatch: %+v", r)
		}
		if r := byPriority[2]; len(r.Matches) != 1 || len(r.Matches[0].QueryParams) != 1 ||
			r.Matches[0].QueryParams[0].Name != "gcp-iap-mode" ||
			r.Matches[0].QueryParams[0].Present == nil || !*r.Matches[0].QueryParams[0].Present {
			return fmt.Errorf("priority 2 rule mismatch: %+v", r)
		}
		if r := byPriority[3]; len(r.Matches) != 1 || len(r.Matches[0].Headers) != 1 ||
			r.Matches[0].Headers[0].Name != "Cookie" ||
			r.Matches[0].Headers[0].Regex != ".*GCP_IAA?P_AUTH_TOKEN.*" {
			return fmt.Errorf("priority 3 rule mismatch: %+v", r)
		}
		if r := byPriority[4]; r.Redirect == nil || r.Redirect.Path != "/signin" ||
			r.Redirect.ResponseCode != redirectCode || r.Redirect.StripQuery {
			return fmt.Errorf("priority 4 rule mismatch: %+v", r)
		}
		return nil
	}
}

func testAccCheckRouteRulesAbsent(name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		entry, err := fetchLiveEntry(name)
		if err != nil {
			return err
		}
		if len(entry.RouteRules) != 0 {
			return fmt.Errorf("expected route rules cleared on %q, got %d", name, len(entry.RouteRules))
		}
		return nil
	}
}

// fetchLiveEntry reads the entry's spec from the live URL map.
func fetchLiveEntry(name string) (entrySpec, error) {
	ref, err := parseURLMapRef(os.Getenv(envURLMap))
	if err != nil {
		return entrySpec{}, err
	}
	got, err := testURLMapsClient.Get(context.Background(), &computepb.GetUrlMapRequest{
		Project: ref.Project,
		UrlMap:  ref.Name,
	})
	if err != nil {
		return entrySpec{}, err
	}
	entry, ok := findEntry(got, name)
	if !ok {
		return entrySpec{}, fmt.Errorf("entry %q not present in %s", name, ref)
	}
	return entry, nil
}

// importIDFunc builds the import identifier from the live state: the
// canonical url_map path plus a trailing /name.
func importIDFunc(resourceName string) resource.ImportStateIdFunc {
	return func(s *terraform.State) (string, error) {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return "", fmt.Errorf("not found: %s", resourceName)
		}
		return fmt.Sprintf("%s/%s", rs.Primary.Attributes["url_map"], rs.Primary.Attributes["name"]), nil
	}
}

func testAccBasicConfig(name, hostSuffix string, includeAlt bool) string {
	hosts := []string{fmt.Sprintf("%q", name+".example.test")}
	if includeAlt {
		hosts = append(hosts, fmt.Sprintf("%q", name+"-renamed.example.test"))
	}
	service := os.Getenv(envBackendService)
	if hostSuffix == "alt" {
		service = os.Getenv(envAltBackendService)
	}

	return fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "test" {
  url_map         = %q
  name            = %q
  hosts           = [%s]
  default_service = %q
  description     = "acctest %s"
}
`,
		os.Getenv(envURLMap),
		name,
		strings.Join(hosts, ", "),
		service,
		hostSuffix,
	)
}

func testAccTwoEntriesConfig(a, b string) string {
	return fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "a" {
  url_map         = %[1]q
  name            = %[2]q
  hosts           = [%[3]q]
  default_service = %[4]q
}

resource "crugcp_compute_url_map_host_rule" "b" {
  url_map         = %[1]q
  name            = %[5]q
  hosts           = [%[6]q]
  default_service = %[4]q
}
`,
		os.Getenv(envURLMap),
		a,
		a+".example.test",
		os.Getenv(envBackendService),
		b,
		b+".example.test",
	)
}

func testAccOneEntryConfig(b string) string {
	return fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "b" {
  url_map         = %[1]q
  name            = %[2]q
  hosts           = [%[3]q]
  default_service = %[4]q
}
`,
		os.Getenv(envURLMap),
		b,
		b+".example.test",
		os.Getenv(envBackendService),
	)
}

// testAccPathRulesConfig renders a single entry with an optional
// path_rules block. The rule's service reuses envBackendService so the
// test needs only one backend.
func testAccPathRulesConfig(name string, paths []string, includePathRules bool) string {
	service := os.Getenv(envBackendService)

	pathRules := ""
	if includePathRules {
		quoted := make([]string, 0, len(paths))
		for _, p := range paths {
			quoted = append(quoted, fmt.Sprintf("%q", p))
		}
		pathRules = fmt.Sprintf(`
  path_rules = [
    {
      paths   = [%s]
      service = %q
    },
  ]
`, strings.Join(quoted, ", "), service)
	}

	return fmt.Sprintf(`
provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "test" {
  url_map         = %[1]q
  name            = %[2]q
  hosts           = [%[3]q]
  default_service = %[4]q
%[5]s}
`,
		os.Getenv(envURLMap),
		name,
		name+".example.test",
		service,
		pathRules,
	)
}

// testAccCheckURLMapEntryExists hits the live URL map via the SDK to
// confirm the entry survived the apply.
func testAccCheckURLMapEntryExists(name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		ref, err := parseURLMapRef(os.Getenv(envURLMap))
		if err != nil {
			return err
		}
		got, err := testURLMapsClient.Get(context.Background(), &computepb.GetUrlMapRequest{
			Project: ref.Project,
			UrlMap:  ref.Name,
		})
		if err != nil {
			return err
		}
		if _, ok := findEntry(got, name); !ok {
			return fmt.Errorf("entry %q not present in %s", name, ref)
		}
		return nil
	}
}

// testAccCheckPathRulePaths reads the live URL map and asserts the
// single path rule's paths match wantPaths as a set — order-independent,
// which is the whole point of modelling paths as a set. It fails if the
// live API dropped, reordered into a mismatch, or added paths.
func testAccCheckPathRulePaths(name string, wantPaths ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		ref, err := parseURLMapRef(os.Getenv(envURLMap))
		if err != nil {
			return err
		}
		got, err := testURLMapsClient.Get(context.Background(), &computepb.GetUrlMapRequest{
			Project: ref.Project,
			UrlMap:  ref.Name,
		})
		if err != nil {
			return err
		}
		entry, ok := findEntry(got, name)
		if !ok {
			return fmt.Errorf("entry %q not present in %s", name, ref)
		}
		if len(entry.PathRules) != 1 {
			return fmt.Errorf("expected exactly one path rule on %q, got %d", name, len(entry.PathRules))
		}
		want := make(map[string]struct{}, len(wantPaths))
		for _, p := range wantPaths {
			want[p] = struct{}{}
		}
		gotPaths := make(map[string]struct{}, len(entry.PathRules[0].Paths))
		for _, p := range entry.PathRules[0].Paths {
			gotPaths[p] = struct{}{}
		}
		if len(want) != len(gotPaths) {
			return fmt.Errorf("path set mismatch for %q: want %v, got %v", name, wantPaths, entry.PathRules[0].Paths)
		}
		for p := range want {
			if _, ok := gotPaths[p]; !ok {
				return fmt.Errorf("path %q missing from %q: got %v", p, name, entry.PathRules[0].Paths)
			}
		}
		return nil
	}
}

func testAccCheckURLMapEntryAbsent(name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		ref, _ := parseURLMapRef(os.Getenv(envURLMap))
		got, err := testURLMapsClient.Get(context.Background(), &computepb.GetUrlMapRequest{
			Project: ref.Project,
			UrlMap:  ref.Name,
		})
		if err != nil {
			return err
		}
		if _, ok := findEntry(got, name); ok {
			return fmt.Errorf("entry %q still present in %s", name, ref)
		}
		return nil
	}
}

func testAccCheckURLMapEntryDestroyed(name string) resource.TestCheckFunc {
	return testAccCheckURLMapEntryAbsent(name)
}
