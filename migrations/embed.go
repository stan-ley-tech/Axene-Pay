// Package migrations embeds the SQL schema migrations (goose format) so that
// binaries and tests apply exactly the files in this directory.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
