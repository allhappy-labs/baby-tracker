//go:build identitytest

// Package postgres exposes the canonical bootstrap only to synthetic tools.
package postgres

import _ "embed"

//go:embed init-roles.sql
var BootstrapSQL string
