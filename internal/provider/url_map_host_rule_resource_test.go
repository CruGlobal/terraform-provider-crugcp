package provider

import (
	"context"
	"fmt"
	"os"
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
