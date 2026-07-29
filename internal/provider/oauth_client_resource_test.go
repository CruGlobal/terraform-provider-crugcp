package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccIAMOAuthClient_placeholder is the whole point of the
// resource: a config whose redirect URI embeds the client's own
// generated id converges in ONE apply, with no intermediate edit.
//
// The Check asserts the resolved URI actually contains the generated
// client_id, so a silently-unsubstituted placeholder fails the test
// rather than shipping a client that can't complete a sign-in.
func TestAccIAMOAuthClient_placeholder(t *testing.T) {
	name := acctestOAuthClientID("p")
	project := os.Getenv(envProject)
	const addr = "crugcp_iam_oauth_client.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheckOAuthClient(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckOAuthClientDestroyed(project, name),
		Steps: []resource.TestStep{
			{
				Config: testAccOAuthClientConfig(project, name, "IAP workforce", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "oauth_client_id", name),
					resource.TestCheckResourceAttr(addr, "location", "global"),
					resource.TestCheckResourceAttr(addr, "disabled", "false"),
					resource.TestCheckResourceAttr(addr, "state", "ACTIVE"),
					resource.TestCheckResourceAttr(addr, "name",
						fmt.Sprintf("projects/%s/locations/global/oauthClients/%s", project, name)),
					resource.TestCheckResourceAttrSet(addr, "client_id"),

					// State keeps the placeholder the config was written with...
					resource.TestCheckResourceAttr(addr, "allowed_redirect_uris.#", "1"),
					resource.TestCheckResourceAttr(addr, "allowed_redirect_uris.0",
						"https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect"),

					// ...while the effective list carries the real id.
					resource.TestCheckResourceAttr(addr, "effective_allowed_redirect_uris.#", "1"),
					testAccCheckRedirectURIResolved(addr),
					testAccCheckOAuthClientRedirectURIs(project, name),
				),
			},
			{
				// A second plan against the unchanged config must be
				// empty — proof the placeholder round-trip doesn't
				// leave a permanent diff behind.
				Config:   testAccOAuthClientConfig(project, name, "IAP workforce", ""),
				PlanOnly: true,
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     fmt.Sprintf("projects/%s/locations/global/oauthClients/%s", project, name),
			},
			{
				// Mutable fields only: no replacement, and the
				// resolved URI must survive untouched.
				Config: testAccOAuthClientConfig(project, name, "IAP workforce renamed", "used by beacon-stage"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "display_name", "IAP workforce renamed"),
					resource.TestCheckResourceAttr(addr, "description", "used by beacon-stage"),
					testAccCheckRedirectURIResolved(addr),
					testAccCheckOAuthClientRedirectURIs(project, name),
				),
			},
		},
	})
}

// TestAccIAMOAuthClient_extraRedirectURI exercises an update that
// changes allowed_redirect_uris itself — the path where the resource
// has to re-resolve the placeholder from state rather than from a
// fresh create response.
func TestAccIAMOAuthClient_extraRedirectURI(t *testing.T) {
	name := acctestOAuthClientID("e")
	project := os.Getenv(envProject)
	const addr = "crugcp_iam_oauth_client.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheckOAuthClient(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckOAuthClientDestroyed(project, name),
		Steps: []resource.TestStep{
			{
				Config: testAccOAuthClientConfig(project, name, "IAP workforce", ""),
				Check:  testAccCheckRedirectURIResolved(addr),
			},
			{
				Config: testAccOAuthClientTwoURIsConfig(project, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "allowed_redirect_uris.#", "2"),
					resource.TestCheckResourceAttr(addr, "allowed_redirect_uris.1",
						"https://example.test/oauth2/callback"),
					resource.TestCheckResourceAttr(addr, "effective_allowed_redirect_uris.#", "2"),
					testAccCheckRedirectURIResolved(addr),
					testAccCheckOAuthClientRedirectURIs(project, name),
				),
			},
			{
				Config:   testAccOAuthClientTwoURIsConfig(project, name),
				PlanOnly: true,
			},
		},
	})
}

// TestAccIAMOAuthClient_noPlaceholder covers the ordinary path: a
// config with nothing to substitute must skip the follow-up PATCH and
// behave like any other resource.
func TestAccIAMOAuthClient_noPlaceholder(t *testing.T) {
	name := acctestOAuthClientID("n")
	project := os.Getenv(envProject)
	const addr = "crugcp_iam_oauth_client.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { preCheckOAuthClient(t) },
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             testAccCheckOAuthClientDestroyed(project, name),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "crugcp_iam_oauth_client" "test" {
  project         = %[1]q
  oauth_client_id = %[2]q
  display_name    = "no placeholder"
  client_type     = "CONFIDENTIAL_CLIENT"

  allowed_grant_types   = ["AUTHORIZATION_CODE_GRANT"]
  allowed_scopes        = ["https://www.googleapis.com/auth/cloud-platform"]
  allowed_redirect_uris = ["https://example.test/oauth2/callback"]
}
`, project, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "allowed_redirect_uris.0", "https://example.test/oauth2/callback"),
					resource.TestCheckResourceAttr(addr, "effective_allowed_redirect_uris.0", "https://example.test/oauth2/callback"),
					testAccCheckOAuthClientRedirectURIs(project, name),
				),
			},
		},
	})
}

// acctestOAuthClientID builds an id that satisfies the API's naming
// rules. It must be unique per run: GCP reserves the name of a deleted
// OAuth client for about 30 days, so reusing a fixed id would make the
// suite fail for a month after its first run.
func acctestOAuthClientID(kind string) string {
	return fmt.Sprintf("crugcp-acc-%s-%s", kind, strings.ToLower(acctest.RandString(8)))
}

func testAccOAuthClientConfig(project, name, displayName, description string) string {
	descLine := ""
	if description != "" {
		descLine = fmt.Sprintf("  description = %q\n", description)
	}
	return fmt.Sprintf(`
resource "crugcp_iam_oauth_client" "test" {
  project         = %[1]q
  oauth_client_id = %[2]q
  display_name    = %[3]q
%[4]s  client_type     = "CONFIDENTIAL_CLIENT"

  allowed_grant_types = ["AUTHORIZATION_CODE_GRANT"]
  allowed_scopes      = ["https://www.googleapis.com/auth/cloud-platform"]

  allowed_redirect_uris = [
    "https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect",
  ]
}
`, project, name, displayName, descLine)
}

func testAccOAuthClientTwoURIsConfig(project, name string) string {
	return fmt.Sprintf(`
resource "crugcp_iam_oauth_client" "test" {
  project         = %[1]q
  oauth_client_id = %[2]q
  display_name    = "IAP workforce"
  client_type     = "CONFIDENTIAL_CLIENT"

  allowed_grant_types = ["AUTHORIZATION_CODE_GRANT"]
  allowed_scopes      = ["https://www.googleapis.com/auth/cloud-platform"]

  allowed_redirect_uris = [
    "https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect",
    "https://example.test/oauth2/callback",
  ]
}
`, project, name)
}

// testAccCheckRedirectURIResolved asserts against Terraform state that
// the effective URI really embeds the generated client_id — the exact
// thing the upstream resource can't do in one apply.
func testAccCheckRedirectURIResolved(addr string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("resource %s not found in state", addr)
		}
		clientID := rs.Primary.Attributes["client_id"]
		if clientID == "" {
			return fmt.Errorf("client_id is empty in state")
		}
		want := fmt.Sprintf("https://iap.googleapis.com/v1/oauth/clientIds/%s:handleRedirect", clientID)
		got := rs.Primary.Attributes["effective_allowed_redirect_uris.0"]
		if got != want {
			return fmt.Errorf("effective_allowed_redirect_uris.0 = %q, want %q", got, want)
		}
		if strings.Contains(got, clientIDPlaceholder) {
			return fmt.Errorf("placeholder survived into the effective URI: %q", got)
		}
		if strings.Contains(got, provisionalClientID) {
			return fmt.Errorf("provisional id survived into the effective URI: %q", got)
		}
		return nil
	}
}

// testAccCheckOAuthClientRedirectURIs goes past Terraform state to the
// live API: GCP must hold exactly what effective_allowed_redirect_uris
// claims it does.
func testAccCheckOAuthClientRedirectURIs(project, name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		ref := oauthClientRef{Project: project, Location: "global", ClientID: name}
		got, err := testIAMService.Projects.Locations.OauthClients.
			Get(ref.String()).Context(context.Background()).Do()
		if err != nil {
			return fmt.Errorf("reading %s: %w", ref, err)
		}
		if got.ClientId == "" {
			return fmt.Errorf("%s has no generated client_id", ref)
		}
		for _, uri := range got.AllowedRedirectUris {
			if strings.Contains(uri, provisionalClientID) {
				return fmt.Errorf("%s still holds the provisional redirect URI %q", ref, uri)
			}
			if strings.Contains(uri, clientIDPlaceholder) {
				return fmt.Errorf("%s still holds an unsubstituted placeholder in %q", ref, uri)
			}
		}
		return nil
	}
}

// testAccCheckOAuthClientDestroyed accepts either outcome GCP offers
// after a delete: the client is gone, or it lingers in the DELETED
// state awaiting purge.
//
// It polls, because the IAM API's reads lag its writes by a few hundred
// milliseconds in both directions — the same window the resource's
// Create has to retry through. A single immediate check reads back the
// pre-delete ACTIVE client and fails spuriously.
func testAccCheckOAuthClientDestroyed(project, name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		ref := oauthClientRef{Project: project, Location: "global", ClientID: name}

		var lastState string
		for attempt := 0; attempt < 6; attempt++ {
			if attempt > 0 {
				time.Sleep(250 * time.Millisecond * (1 << (attempt - 1)))
			}
			got, err := testIAMService.Projects.Locations.OauthClients.
				Get(ref.String()).Context(context.Background()).Do()
			if err != nil {
				if isNotFound(err) {
					return nil
				}
				return fmt.Errorf("checking destruction of %s: %w", ref, err)
			}
			if got.State == "DELETED" {
				return nil
			}
			lastState = got.State
		}
		return fmt.Errorf("%s still exists in state %q after destroy", ref, lastState)
	}
}
