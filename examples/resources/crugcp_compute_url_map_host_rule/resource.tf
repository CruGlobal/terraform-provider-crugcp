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
