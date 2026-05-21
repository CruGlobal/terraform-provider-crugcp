terraform {
  required_providers {
    crugcp = {
      source  = "CruGlobal/crugcp"
      version = "~> 0.1"
    }
  }
}

# Default — uses Application Default Credentials.
provider "crugcp" {}

# Or set explicit credentials and a request reason for audit logs.
# provider "crugcp" {
#   credentials                 = file("~/.config/gcloud/svc.json")
#   impersonate_service_account = "tf-shared-lb@cru-shared-cloudrun-lb.iam.gserviceaccount.com"
#   request_timeout             = "60s"
#   request_reason              = "terraform-apply"
# }
