# Import ID is the client's full resource name. allowed_redirect_uris
# comes back with the generated client_id folded into {clientid}, so an
# imported client matches a hand-written config.
terraform import crugcp_iam_oauth_client.iap \
  projects/cru-beacon-stage/locations/global/oauthClients/iap-workforce
