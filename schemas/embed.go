// Package schemas embeds the strict JSON Schemas that validate Artifact
// bodies (design 10.2, PRD 已确认：Forward-only SQLite Migration 与不可变
// Artifact Schema 约束 10). Each file is a validation contract for one
// Artifact body: the Artifact Store validates every agent-authored body
// against its embedded schema before redaction and canonical serialization;
// the Workflow Compiler validates Patch IR against workflow-patch.json.
//
// The files are embedded policy and immutable once released: a schema
// change creates a new schema revision and, through the normal Workflow
// Revision gates, new derived Artifacts — it never edits an existing
// Artifact in place (design 10.3).
package schemas

import "embed"

// FS embeds every strict JSON Schema in this directory.
//
//go:embed *.json
var FS embed.FS
