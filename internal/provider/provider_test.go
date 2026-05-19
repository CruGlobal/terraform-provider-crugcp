package provider

import (
	"context"
	"os"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// protoV6ProviderFactories is wired into every resource.TestCase. The
// acceptance test process re-uses one in-memory provider instance per
// test so the URL-map client doesn't get re-built between steps.
var protoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"crugcp": providerserver.NewProtocol6WithError(New("acctest")()),
}

// testProject is the GCP project under which the acceptance tests
// create and tear down their URL maps. Required for any TF_ACC run.
const envProject = "CRUGCP_ACC_PROJECT"

// testURLMap names a pre-existing URL map in envProject that the tests
// splice entries into. Acceptance tests do NOT create the URL map
// themselves — provisioning a global ALB is slow ($$$ on every run)
// and the resource under test deliberately doesn't own the parent.
const envURLMap = "CRUGCP_ACC_URL_MAP"

// testBackendService points at a backend resource the tests can route
// to in `default_service`. Any valid backend the parent URL map can
// reach is fine (typically a placeholder serverless NEG).
const envBackendService = "CRUGCP_ACC_BACKEND_SERVICE"

// testAltBackendService is used by the "update default_service" step.
// It must differ from testBackendService.
const envAltBackendService = "CRUGCP_ACC_ALT_BACKEND_SERVICE"

func preCheck(t *testing.T) {
	t.Helper()
	for _, v := range []string{envProject, envURLMap, envBackendService, envAltBackendService} {
		if os.Getenv(v) == "" {
			t.Fatalf("%s must be set for acceptance tests", v)
		}
	}
}

// testURLMapsClient returns a Compute URL Maps client built with the
// same auth defaults as the provider — used in CheckDestroy to confirm
// entries really do disappear from the URL map after the resource is
// removed.
var testURLMapsClient *compute.UrlMapsClient

func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC") == "" {
		// Unit tests don't need the client; skip the setup.
		os.Exit(m.Run())
	}

	ctx := context.Background()
	client, err := compute.NewUrlMapsRESTClient(ctx)
	if err != nil {
		panic(err)
	}
	testURLMapsClient = client
	code := m.Run()
	_ = client.Close()
	os.Exit(code)
}
