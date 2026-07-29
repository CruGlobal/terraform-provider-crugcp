# The OAuth client IAP uses to sign users in through a workforce
# identity federation pool.
#
# GCP's documented flow for this is two applies: create the client with
# a dummy redirect URI, read back the generated client_id, paste it
# into the config, apply again. The `{clientid}` placeholder collapses
# that into one apply — the provider substitutes the generated id
# immediately after create.
resource "crugcp_iam_oauth_client" "iap" {
  project         = "cru-beacon-stage"
  oauth_client_id = "iap-workforce"
  display_name    = "Beacon stage IAP"
  description     = "Sign-in client for IAP + Okta via workforce identity federation"
  client_type     = "CONFIDENTIAL_CLIENT"

  allowed_grant_types = ["AUTHORIZATION_CODE_GRANT"]
  allowed_scopes      = ["https://www.googleapis.com/auth/cloud-platform"]

  allowed_redirect_uris = [
    "https://iap.googleapis.com/v1/oauth/clientIds/{clientid}:handleRedirect",
  ]
}

# The client secret stays with the upstream provider, and needs no
# placeholder trickery — the generated client_id is an ordinary
# reference once the client above exists.
resource "google_iam_oauth_client_credential" "iap" {
  project                    = crugcp_iam_oauth_client.iap.project
  location                   = crugcp_iam_oauth_client.iap.location
  oauthclient                = crugcp_iam_oauth_client.iap.oauth_client_id
  oauth_client_credential_id = "iap-secret"
  display_name               = "Beacon stage IAP secret"
}

# Wire the client into IAP's workforce identity settings. Because the
# redirect URI resolved itself during create, this whole graph — pool,
# client, secret, settings — converges in a single apply.
resource "google_iap_settings" "beacon_stage" {
  name = "projects/${data.google_project.beacon_stage.number}/iap_web/compute"

  access_settings {
    identity_sources = ["WORKFORCE_IDENTITY_FEDERATION"]

    workforce_identity_settings {
      workforce_pools = [google_iam_workforce_pool.beacon_stage.name]

      oauth2 {
        client_id     = crugcp_iam_oauth_client.iap.client_id
        client_secret = google_iam_oauth_client_credential.iap.client_secret
      }
    }
  }
}

# What GCP actually stores, with the placeholder resolved. Useful for
# eyeballing the substitution after an apply.
output "iap_redirect_uris" {
  value = crugcp_iam_oauth_client.iap.effective_allowed_redirect_uris
}
