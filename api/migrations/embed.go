// Package migrations embeds the SQL migration files into the binary.
//
// The Go file has to live in the same directory as the .sql files: //go:embed
// patterns cannot reach outside the package directory with "..", by design.
//
// Embedding rather than reading from disk means the deployed binary carries its
// own migrations, so a Fly.io release command is just `api migrate up` with no
// separate file copy or migration image to keep in sync.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
