// Package layout implements the aggregated workflow directory resolver
// (TUI workflow design §7) and the explicit Legacy Layout Migration
// (design §7.4, TUI task 8): a Layout Version 1 workflow's Artifacts and
// Worktrees are moved into the aggregated <workflow-id>/ root, and the
// persisted Layout facts advance to Version 2. The migration is explicit
// (Preview → Prepare → Execute), read-only until the user confirms, and
// every move is either a managed `git worktree move` or a safe path move
// under the managed home.
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"

	"cflow.local/cflow/internal/model"
)

// MoveKind is one migration move kind.
// MoveKind aliases the model move kind for the public migration API.
type MoveKind = model.PathMoveKind

const (
	// MoveKindWorktree moves one managed Git Worktree with
	// `git worktree move` (the worktree registry and its branch follow).
	MoveKindWorktree = model.MoveKindWorktree
	// MoveKindArtifact moves one managed directory or file with a safe
	// path move under the managed home (never a foreign path).
	MoveKindArtifact = model.MoveKindArtifact
)

// PathMove is one exact source→destination move of a migration. Branch
// and Head bind a Worktree move to its registered identity; Head binds an
// Artifact move to the manifest evidence.
type PathMove = model.PathMove

// MigrationPreview is the read-only projection of one Workflow's Legacy
// Layout Migration: the exact ordered moves and the canonical manifest
// hash the Prepare step persists and the Execute step binds. Computing a
// Preview never moves, creates, or deletes anything.
type MigrationPreview struct {
	Workflow model.WorkflowID `json:"workflow"`
	// From is the current Layout Version (1 = legacy).
	From int `json:"from"`
	// To is the target Layout Version (2 = aggregated workspace).
	To int `json:"to"`
	// Moves is the deterministic ordered move list.
	Moves []PathMove `json:"moves"`
	// ManifestHash is the canonical SHA-256 of the ordered moves.
	ManifestHash string `json:"manifest_hash"`
}

// Preview computes the deterministic Legacy Layout Migration moves of one
// Layout Version 1 workflow from the legacy roots into the aggregated
// workflow root. It is a pure function of the paths and the recorded
// facts: reading it never touches the filesystem.
//
//   - the legacy Artifacts root projects/<key>/workflows/<wf>/artifacts
//     moves into the aggregated root's artifacts/;
//   - the legacy Planning Snapshot worktrees/<key>/<wf>/planning moves to
//     the aggregated temporary root tmp/planning;
//   - the legacy Integration Worktree worktrees/<key>/<wf>/integration —
//     the delivery mainline of the legacy layout — moves to the
//     aggregated workspace (its integration Branch becomes the Workspace
//     Branch, recorded by the migration);
//   - every legacy Task Worktree worktrees/<key>/<wf>/tasks/<node> moves
//     to the aggregated temporary root tmp/tasks/<node>.
//
// A workflow that is not on Layout 1, or a move list that cannot be
// derived (an empty integration head), fails closed without a preview.
func Preview(wf model.WorkflowID, from, to int, projectKey, home string, st model.State) (MigrationPreview, error) {
	if from != 1 || to != 2 {
		return MigrationPreview{}, model.InvalidInputFault("layout migration requires Layout 1 -> Layout 2")
	}
	if wf == "" || projectKey == "" || home == "" {
		return MigrationPreview{}, model.InvalidInputFault("layout migration requires the workflow, project, and home identity")
	}
	if err := (Resolver{Home: home, ProjectKey: projectKey}).Validate(); err != nil {
		return MigrationPreview{}, err
	}
	if st.Workflow.ID != wf || st.Workflow.LayoutVersion != from {
		return MigrationPreview{}, model.InvalidInputFault(
			"the workflow is not a Layout "+itoa(from)+" workflow; nothing to migrate")
	}
	if st.Workflow.IntegrationHead == "" {
		return MigrationPreview{}, model.InvalidInputFault(
			"the legacy workflow has no recorded integration head; its delivery cannot be migrated")
	}
	key := projectKey
	legacyRoot := filepath.Join(home, "worktrees", key, string(wf))
	agg := Resolver{Home: home, ProjectKey: key}
	aggRoot := agg.WorkflowRoot(wf)

	moves := []PathMove{
		// The legacy Planning Snapshot becomes the aggregated temporary
		// planning root (a detached snapshot; never the delivery).
		{
			Kind: model.MoveKindWorktree, Source: filepath.Join(legacyRoot, "planning"),
			Destination: filepath.Join(aggRoot, "tmp", "planning"),
			Head:        st.Workflow.BaseCommit,
		},
	}
	// The legacy Integration Worktree becomes the aggregated Workspace;
	// its Branch becomes the Workspace Branch.
	if st.Workflow.IntegrationBranch != "" {
		moves = append(moves, PathMove{
			Kind: model.MoveKindWorktree, Source: filepath.Join(legacyRoot, "integration"),
			Destination: agg.Workspace(wf),
			Branch:      st.Workflow.IntegrationBranch,
			Head:        st.Workflow.IntegrationHead,
		})
	}
	// Every legacy Task Worktree moves into the aggregated temporary
	// tasks root.
	taskIDs := taskIDsOf(st)
	for _, node := range taskIDs {
		moves = append(moves, PathMove{
			Kind: model.MoveKindWorktree, Source: filepath.Join(legacyRoot, "tasks", string(node)),
			Destination: agg.Task(wf, node),
			Branch:      "cflow/" + string(wf) + "/task-" + string(node),
		})
	}
	// The legacy Artifacts root moves into the aggregated root.
	moves = append(moves, PathMove{
		Kind:        MoveKindArtifact,
		Source:      filepath.Join(home, "projects", key, "workflows", string(wf), "artifacts"),
		Destination: filepath.Join(aggRoot, "artifacts"),
	})
	// The static workflow.yaml manifest follows the Artifacts.
	moves = append(moves, PathMove{
		Kind:        MoveKindArtifact,
		Source:      filepath.Join(home, "projects", key, "workflows", string(wf), "workflow.yaml"),
		Destination: filepath.Join(aggRoot, "workflow.yaml"),
	})

	preview := MigrationPreview{
		Workflow: wf, From: from, To: to, Moves: moves,
	}
	preview.ManifestHash = preview.Hash()
	return preview, nil
}

// Hash returns the canonical manifest hash of the ordered moves.
func (p MigrationPreview) Hash() string {
	data, err := json.Marshal(struct {
		Workflow model.WorkflowID `json:"workflow"`
		From     int              `json:"from"`
		To       int              `json:"to"`
		Moves    []PathMove       `json:"moves"`
	}{p.Workflow, p.From, p.To, p.Moves})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// taskIDsOf returns the deterministic sorted Node ids of the legacy Task
// Worktrees of one workflow (the aggregated task root needs them).
func taskIDsOf(st model.State) []model.NodeID {
	ids := make([]model.NodeID, 0, len(st.Nodes))
	for id, n := range st.Nodes {
		if n == nil || n.Kind != model.NodeAgentTask {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return string(ids[i]) < string(ids[j]) })
	return ids
}

// itoa renders a small integer.
func itoa(n int) string {
	return strconv.Itoa(n)
}
