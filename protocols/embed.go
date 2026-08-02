// Package protocols embeds the Provider protocol bindings (design 14.2).
// The Agent Runtime's Provider Registry parses providers.yaml into
// immutable binding entries; every start/resume/fallback compares against
// the exact binding approved for its route. OpenCode may appear only as
// disabled P1 metadata and can never be selected.
//
// The file is embedded policy and immutable once released.
package protocols

import "embed"

// FS embeds the Provider protocol bindings file.
//
//go:embed providers.yaml
var FS embed.FS
