resource "crugcp_compute_url_map_host_rule" "app_stage" {
  url_map         = "projects/cru-shared-cloudrun-lb/global/urlMaps/internal-shared"
  name            = "app-stage"
  hosts           = ["app-stage.gcp.cru.org"]
  default_service = "projects/app-stage-4km3/regions/us-central1/networkEndpointGroups/serverless-neg"
  description     = "App stage"

  # Route /api/* to a second serverless NEG; everything else falls
  # through to default_service.
  path_rules = [
    {
      paths   = ["/api", "/api/*"]
      service = "projects/app-stage-4km3/regions/us-central1/networkEndpointGroups/api-neg"
    },
  ]
}

# route_rules (mutually exclusive with path_rules) unlock header and
# query-parameter matching plus redirects. This example implements a
# friendly sign-in page for an IAP-protected app: anonymous visitors
# are redirected to a static /signin page, while requests carrying an
# IAP session cookie (or mid-auth-flow query params) reach the app.
# Requires an EXTERNAL_MANAGED load balancer.
resource "crugcp_compute_url_map_host_rule" "app_prod" {
  url_map = "projects/cru-shared-cloudrun-lb/global/urlMaps/internal-shared"
  name    = "app-prod"
  hosts   = ["app.gcp.cru.org"]

  route_rules = [
    {
      # Public sign-in page and its assets, served from a GCS backend.
      priority = 1
      match    = [{ prefix = "/signin" }, { prefix = "/assets/" }]
      service  = "projects/app-prod-4km3/global/backendServices/signin-bucket"
    },
    {
      # IAP's auth callback: it 302s back to the original URL with
      # ?gcp-iap-mode=... appended.
      priority = 2
      match = [{
        prefix       = "/"
        query_params = [{ name = "gcp-iap-mode", present = true }]
      }]
      service = "projects/app-prod-4km3/regions/us-central1/networkEndpointGroups/serverless-neg"
    },
    {
      # The sign-in button links to /?login=1 to enter the IAP flow.
      priority = 3
      match = [{
        prefix       = "/"
        query_params = [{ name = "login", present = true }]
      }]
      service = "projects/app-prod-4km3/regions/us-central1/networkEndpointGroups/serverless-neg"
    },
    {
      # Returning users: an IAP session cookie routes straight to the
      # app. Cookie presence is not validity — a stale cookie just
      # gets seamlessly re-authenticated by IAP.
      priority = 4
      match = [{
        prefix  = "/"
        headers = [{ name = "Cookie", regex = ".*GCP_IAA?P_AUTH_TOKEN.*" }]
      }]
      service = "projects/app-prod-4km3/regions/us-central1/networkEndpointGroups/serverless-neg"
    },
    {
      # Everyone else lands on the sign-in page. Use a temporary
      # redirect — browsers cache permanent ones aggressively.
      priority = 5
      match    = [{ prefix = "/" }]
      redirect = {
        path          = "/signin"
        response_code = "FOUND"
        strip_query   = false
      }
    },
  ]
}
