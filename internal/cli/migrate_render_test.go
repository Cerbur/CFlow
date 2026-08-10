package cli

// The migration Preview render (finding 6) must show the complete
// evidence: the authoritative migration row/status, the immutable
// manifest and backup identity, the exact database impact, and every move
// bound to its branch/head/digest.

import (
	"bytes"
	"strings"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

func TestRenderMigrationPreviewShowsCompleteEvidence(t *testing.T) {
	v := app.MigrationPreviewView{
		Workflow: "wf-legacy", From: 1, To: 2, Status: "PREPARED",
		MigrationID: "migration-wf-legacy-abc123", ManifestHash: "manifest-hash-1",
		ManifestPath: "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.json",
		BackupPath:   "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.db.backup",
		BackupHash:   "backup-hash-1", BackupSize: 4096,
		SourceSnapshotHash:      "snapshot-hash-1",
		ExpectedWorkspacePath:   "/cflow/projects/p/wf-legacy/workspace",
		ExpectedWorkspaceBranch: "cflow/wf-legacy/integration",
		ExpectedWorkspaceHead:   "1111111111111111111111111111111111111111",
		Moves: []model.PathMove{
			{Kind: model.MoveKindWorktree, Source: "/cflow/worktrees/p/wf-legacy/integration",
				Destination: "/cflow/projects/p/wf-legacy/workspace",
				Branch: "cflow/wf-legacy/integration", Head: "1111111111111111111111111111111111111111"},
			{Kind: model.MoveKindArtifact, Source: "/cflow/projects/p/wf-legacy/workflows/wf-legacy/artifacts",
				Destination: "/cflow/projects/p/wf-legacy/artifacts", Digest: "digest-1"},
		},
	}
	var buf bytes.Buffer
	renderMigrationPreview(&buf, v, security.Registry{})
	out := buf.String()
	for _, want := range []string{
		"status: PREPARED",
		"migration id: migration-wf-legacy-abc123",
		"manifest path:",
		"backup:",
		"sha256=backup-hash-1",
		"source snapshot: snapshot-hash-1",
		"database impact: layout 1 -> 2",
		"move 1: worktree",
		"branch=cflow/wf-legacy/integration",
		"digest=digest-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("migration preview output missing %q:\n%s", want, out)
		}
	}
}
