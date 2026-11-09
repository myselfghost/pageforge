// Package migrations embeds SQL migrations for both supported dialects.
package migrations

import "embed"

//go:embed sqlite/*.sql postgres/*.sql
var FS embed.FS
