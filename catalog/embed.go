// Package catalog holds the embedded app definitions (one YAML per app kind).
package catalog

import "embed"

//go:embed *.yaml
var FS embed.FS
