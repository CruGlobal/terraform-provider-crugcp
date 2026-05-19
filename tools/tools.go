//go:build tools

// Package tools pins build-time tooling as actual module dependencies so
// `go mod tidy` keeps them in go.mod even though no production code
// imports them. terraform-plugin-docs is invoked via `go generate` (see
// main.go) rather than as a library, which is invisible to module
// resolution without this trick.
package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)
