// Package migrations embeds the forward-only SQLite migration chain.
//
// The files in this directory are immutable once released: the Store's
// migration registry (internal/store) pins each migration's stable ID and
// SHA-256 digest, and applied content and checksums must never change in a
// later binary (PRD 决策 1). The registry is the single consumer.
package migrations

import "embed"

// FS embeds every migration script in this directory, ordered by their
// numeric prefix. There is intentionally no v001.sql here: that file lives
// in tests/testdata/db and represents a database created by the previous
// binary, kept byte-identical to 001 (asserted by the migration tests).
//
//go:embed *.sql
var FS embed.FS
