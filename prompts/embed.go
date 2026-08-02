// Package prompts embeds the versioned Agent prompt templates (design
// 14.5, PRD Prompt 版本化). Each file begins with a machine-parsed YAML
// header binding purpose, revision, input_schema and output_schema, and is
// addressed by Agent Purpose plus registry revision and content hash.
// Prompts may request structured output but never grant routes,
// permissions, executable commands, budgets, approvals, or lifecycle
// state; updating a prompt creates a new registry revision while
// historical Session records retain their original reference.
//
// The files are embedded policy and immutable once released.
package prompts

import "embed"

// FS embeds every prompt template in this directory.
//
//go:embed *.md
var FS embed.FS
