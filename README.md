# Terraform Provider CruGCP

> **Status: AI-supported, not actively maintained.** Built for an
> internal use case at Cru. Dependabot keeps dependencies and security
> advisories current automatically (patch and minor bumps auto-merge;
> majors require manual review). Feature work and bug fixes happen on
> a best-effort basis. **Pull requests and issues are welcome** — they
> may take time to be reviewed.

`terraform-provider-crugcp` exposes individual entries on a shared
Google Cloud **Compute URL map** as standalone Terraform resources, so
multiple Terraform configurations can each register a host rule on the
same URL map without contending over ownership of the parent resource.

The upstream `google_compute_url_map` resource manages the entire URL
map atomically — there is no supported way for one configuration to
register a single host rule on a map that another configuration owns.
This provider fills that gap, modelled on the same split Google uses
for `google_certificate_manager_certificate_map` (the container) and
`google_certificate_manager_certificate_map_entry` (individual
entries).

The typical use case is amortising the fixed cost of a Global External
ALB across many low-traffic internal apps that each live in their own
GCP project.

## Resources

- `crugcp_compute_url_map_host_rule` — one host rule plus a matching
  path matcher, both keyed by `name`, spliced into a pre-existing
  global URL map. Concurrent writers are reconciled via the Compute
  API's fingerprint-based optimistic locking with an automatic
  read-modify-write retry loop on HTTP 412.

## Requirements

- [Terraform](https://www.terraform.io/downloads.html) >= 1.13
- [Go](https://golang.org/doc/install) >= 1.24 (only for building from
  source)
- A pre-existing global Compute URL map you have `compute.urlMaps.get`
  and `compute.urlMaps.patch` permission on.

## Using the provider

Add it to `required_providers` and configure auth. Application Default
Credentials are picked up automatically; the explicit knobs mirror the
`hashicorp/google` provider's authentication UX.

```hcl
terraform {
  required_providers {
    crugcp = {
      source  = "CruGlobal/crugcp"
      version = "~> 0.1"
    }
  }
}

provider "crugcp" {}

resource "crugcp_compute_url_map_host_rule" "app_stage" {
  url_map         = "projects/cru-shared-cloudrun-lb/global/urlMaps/internal-shared"
  name            = "app-stage"
  hosts           = ["app-stage.gcp.cru.org"]
  default_service = "projects/app-stage-4km3/regions/us-central1/networkEndpointGroups/serverless-neg"
  description     = "App stage"
}
```

### Provider attributes

| Attribute                       | Description                                                                                                          |
| ------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `credentials`                   | Path to a service-account JSON key file, or the JSON contents inline. Falls back to `GOOGLE_*` env vars, then ADC.   |
| `access_token`                  | Short-lived OAuth access token. Mutually exclusive with `credentials` and `impersonate_service_account`.             |
| `impersonate_service_account`   | Service account to impersonate. The principal supplying credentials needs `roles/iam.serviceAccountTokenCreator`.    |
| `request_timeout`               | Go duration string (default `60s`) applied to each Compute API call.                                                 |
| `request_reason`                | Value sent in the `X-Goog-Request-Reason` header; surfaces in GCP audit logs.                                        |

Full reference docs (generated from the provider schema) live in
[`docs/`](./docs/) and on the
[Terraform Registry](https://registry.terraform.io/providers/CruGlobal/crugcp/latest).

### Importing existing entries

```sh
terraform import crugcp_compute_url_map_host_rule.app_stage \
  projects/cru-shared-cloudrun-lb/global/urlMaps/internal-shared/app-stage
```

## Building from source

```sh
git clone https://github.com/CruGlobal/terraform-provider-crugcp
cd terraform-provider-crugcp
go build ./...
```

Pre-built, GPG-signed binaries are produced by goreleaser on every
GitHub Release and published to the public Terraform Registry.

## Developing

Common workflows are defined in [`Taskfile.yaml`](./Taskfile.yaml):

```sh
task build       # compile the provider
task test        # run unit tests
task generate    # regenerate docs from schema (needs terraform on PATH)
task testacc     # run acceptance tests against a real GCP project
```

Acceptance tests cost real GCP API calls. They require a project, a
pre-provisioned URL map, and two backend services (or NEGs) to route
to:

```sh
export TF_ACC=1
export CRUGCP_ACC_PROJECT=...
export CRUGCP_ACC_URL_MAP=projects/.../global/urlMaps/...
export CRUGCP_ACC_BACKEND_SERVICE=projects/.../...
export CRUGCP_ACC_ALT_BACKEND_SERVICE=projects/.../...
task testacc
```

## License

BSD 3-Clause. See [`LICENSE`](./LICENSE).
