package app

// The explicit Legacy Layout Migration (TUI task 8, design §7.4): a
// Layout Version 1 workflow's Artifacts and Worktrees are moved into the
// aggregated <workflow-id>/ root and the persisted Layout facts advance
// to Version 2. The flow is Preview → Prepare → Execute: the Preview is
// read-only, Prepare persists the migration row and the effect Intent
// (exactly recoverable after a crash before any move), and Execute
// performs the ordered moves and then advances the Layout facts in the
// same committed sequence.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/layout"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// migrationPreview derives the full Legacy Layout Migration Preview of
// one workflow: the pure layout.Preview moves plus every legacy Task
// Worktree discovered under worktrees/<key>/<wf>/tasks/ (the aggregate
// does not persist Task Nodes until the graph is installed, so the
// legacy directory is the authoritative inventory). The read is a
// read-only directory listing; nothing is moved, created, or deleted.
func (a *Application) migrationPreview(ctx context.Context, wf model.WorkflowID, state model.State) (layout.MigrationPreview, error) {
	preview, err := layout.Preview(wf, 1, 2, a.project.Key, a.home, state)
	if err != nil {
		return layout.MigrationPreview{}, err
	}
	legacyTasks := filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "tasks")
	entries, err := os.ReadDir(legacyTasks)
	if err != nil {
		return layout.MigrationPreview{}, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		node := model.NodeID(name)
		source := filepath.Join(legacyTasks, name)
		facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: source})
		if err != nil {
			return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"legacy task worktree is unreadable: "+source)
		}
		status, ok := facts.(gitflow.StatusFacts)
		if !ok || status.Head == "" {
			return layout.MigrationPreview{}, model.InvariantFault(
				fmt.Errorf("legacy task worktree has no observable head"))
		}
		preview.Moves = append(preview.Moves, model.PathMove{
			Kind:        model.MoveKindWorktree,
			Source:      source,
			Destination: a.layout.Task(wf, node),
			Branch:      "cflow/" + string(wf) + "/task-" + name,
			Head:        status.Head,
		})
	}
	// Legacy Artifact revisions live below artifacts/<workflow>/<type>.
	// Add one exact move per existing declared Type into the same aggregate
	// category/type directory NewWorkflow reads. Unknown entries block
	// rather than being guessed or copied.
	legacyTypes := filepath.Join(a.home, "projects", a.project.Key, "workflows",
		string(wf), "artifacts", string(wf))
	if artifactEntries, readErr := os.ReadDir(legacyTypes); readErr == nil {
		for _, entry := range artifactEntries {
			if !entry.IsDir() {
				return layout.MigrationPreview{}, model.InvariantFault(
					fmt.Errorf("legacy artifact root contains a non-directory entry %s", entry.Name()))
			}
			typ := model.ArtifactType(entry.Name())
			if !typ.Valid() {
				return layout.MigrationPreview{}, model.InvalidInputFault(
					"legacy artifact root contains an unknown type " + entry.Name())
			}
			destination, err := artifact.WorkflowTypeDir(a.layout.WorkflowRoot(wf), typ)
			if err != nil {
				return layout.MigrationPreview{}, err
			}
			preview.Moves = append(preview.Moves, model.PathMove{
				Kind: model.MoveKindArtifact, Source: filepath.Join(legacyTypes, entry.Name()),
				Destination: destination,
			})
		}
	} else if !os.IsNotExist(readErr) {
		return layout.MigrationPreview{}, readErr
	}
	for i := range preview.Moves {
		if preview.Moves[i].Kind != model.MoveKindArtifact {
			continue
		}
		digest, err := layout.DigestPath(preview.Moves[i].Source)
		if err != nil {
			return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"legacy artifact migration source is missing or unsafe")
		}
		preview.Moves[i].Digest = digest
	}
	preview.ManifestHash = preview.Hash()
	return preview, nil
}

// queryMigrationPreview projects the read-only Legacy Layout Migration
// Preview of one workflow. It reads the aggregate and derives the moves
// from the recorded facts; nothing is created, moved, or deleted.
func (a *Application) queryMigrationPreview(ctx context.Context, q LayoutMigrationPreviewQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return nil, model.InvalidInputFault("no workflow")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	for _, pending := range view.PendingEffects {
		if intent, ok := pending.Intent.(model.LayoutMigrationIntent); ok {
			if intent.Workflow != wf || intent.MigrationID == "" || intent.ManifestHash == "" || len(intent.Moves) == 0 {
				return nil, model.InvariantFault(fmt.Errorf("the persisted layout migration intent is incomplete"))
			}
			return MigrationPreviewView{
				Workflow: wf, From: 1, To: 2, Moves: append([]model.PathMove(nil), intent.Moves...),
				ManifestHash: intent.ManifestHash, MigrationID: intent.MigrationID, Status: "PREPARED",
			}, nil
		}
	}
	if view.State.Workflow.LayoutVersion != 1 {
		return nil, model.InvalidInputFault("the workflow is not on the legacy layout; nothing to migrate")
	}
	preview, err := a.migrationPreview(ctx, wf, view.State)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	return MigrationPreviewView{
		Workflow: preview.Workflow, From: preview.From, To: preview.To,
		Moves: preview.Moves, ManifestHash: preview.ManifestHash, Status: "PREVIEW",
	}, nil
}

// prepareMigration is the prepare-case of PrepareLayoutMigrationCommand:
// it validates the bound Preview against the current facts. The actual
// migration row is recorded by prepareMigrationExecute under the
// mutation lock batch.
func (a *Application) prepareMigration(ctx context.Context, cmd PrepareLayoutMigrationCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(cmd.Workflow)
	if err != nil {
		return nil, "", err
	}
	if cmd.ManifestHash == "" {
		return nil, "", model.InvalidInputFault("the migration requires the exact preview manifest hash")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf || view.State.Workflow.LayoutVersion != 1 {
		return nil, "", model.InvalidInputFault("the workflow is not on the legacy layout; nothing to migrate")
	}
	preview, err := a.migrationPreview(ctx, wf, view.State)
	if err != nil {
		return nil, "", err
	}
	if preview.ManifestHash != cmd.ManifestHash {
		return nil, "", model.NewFault(model.CodeApprovalInputChanged,
			"the migration preview changed since it was displayed; re-preview before preparing")
	}
	return model.ReconcileInput{}, wf, nil
}

// prepareMigrationExecute is the Prepare step under the mutation lock
// batch: it re-validates the Preview, persists the migration row
// (status PREPARED, the canonical manifest hash), and verifies the
// migration sources. No file or Worktree moves during Prepare.
func (a *Application) prepareMigrationExecute(ctx context.Context, st *store.Store, wf model.WorkflowID, cmd PrepareLayoutMigrationCommand) (Outcome, error) {
	if cmd.ManifestHash == "" {
		return Outcome{}, model.InvalidInputFault("the migration requires the exact preview manifest hash")
	}
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	state := view.State
	if state.Workflow.ID != wf || state.Workflow.LayoutVersion != 1 {
		return Outcome{}, model.InvalidInputFault("the workflow is not on the legacy layout; nothing to migrate")
	}
	preview, err := a.migrationPreview(ctx, wf, state)
	if err != nil {
		return Outcome{}, err
	}
	if preview.ManifestHash != cmd.ManifestHash {
		return Outcome{}, model.NewFault(model.CodeApprovalInputChanged,
			"the migration preview changed since it was displayed; re-preview before preparing")
	}
	// The sources must still match the bound manifest: a drift between the
	// Preview and the Prepare is refused with nothing persisted.
	if err := a.verifyMigrationSources(ctx, preview.Moves); err != nil {
		return Outcome{}, err
	}
	id := migrationID(wf, preview.ManifestHash)
	manifestPath := filepath.Join(a.layout.StateDir(wf), "layout-migrations", id+".json")
	if err := a.writeMigrationManifest(ctx, wf, preview, manifestPath); err != nil {
		return Outcome{}, err
	}
	backupPath := strings.TrimSuffix(manifestPath, ".json") + ".db.backup"
	if err := st.BackupLayoutMigration(ctx, backupPath); err != nil {
		return Outcome{}, err
	}
	if err := st.RecordLayoutMigration(ctx, wf, id, manifestPath, preview.ManifestHash); err != nil {
		return Outcome{}, err
	}
	intent := model.LayoutMigrationIntent{
		MigrationID: id, Workflow: wf, ManifestHash: preview.ManifestHash,
		Moves: append([]model.PathMove(nil), preview.Moves...),
	}
	for _, pending := range view.PendingEffects {
		if existing, ok := pending.Intent.(model.LayoutMigrationIntent); ok {
			if existing.MigrationID == id && existing.ManifestHash == preview.ManifestHash {
				return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime}, nil
			}
			return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"a different layout migration intent is already pending")
		}
	}
	if _, err := st.Transact(ctx, view.AggregateVersion, func(s model.State) (model.Decision, error) {
		if s.Workflow.ID != wf || s.Workflow.LayoutVersion != 1 {
			return model.Decision{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"the workflow layout changed while preparing the migration")
		}
		return model.Decision{Effect: intent}, nil
	}); err != nil {
		return Outcome{}, err
	}
	return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime}, nil
}

func migrationID(wf model.WorkflowID, manifestHash string) string {
	short := manifestHash
	if len(short) > 16 {
		short = short[:16]
	}
	return "migration-" + string(wf) + "-" + short
}

// writeMigrationManifest persists the canonical migration manifest (the
// ordered moves) under the workflow's state directory through the
// security guard, so the Execute step and the Recovery engine read the
// exact bound moves.
func (a *Application) writeMigrationManifest(ctx context.Context, wf model.WorkflowID, preview layout.MigrationPreview, path string) error {
	if err := a.ensureWorktreeParent(path); err != nil {
		return err
	}
	data, err := json.Marshal(preview)
	if err != nil {
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be serialized"))
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return model.InvariantFault(fmt.Errorf("migration manifest cannot be persisted"))
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || string(existing) != string(data) {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"the immutable layout migration manifest already exists with different content")
		}
		return nil
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be persisted"))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be synced"))
	}
	if err := f.Close(); err != nil {
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be closed"))
	}
	return nil
}

// executeMigration performs the ordered moves of the bound manifest and
// advances the persisted Layout facts to Version 2. The first pass
// performs every move (git worktree move for the Worktrees, safe path
// moves for the Artifacts); the final Decision records the Layout
// mutation and the migration completion. A crash inside the moves leaves
// the Intent pending with Done counting the completed moves; recovery
// continues from the actual state.
func (a *Application) executeMigration(ctx context.Context, st *store.Store, wf model.WorkflowID, cmd ExecuteLayoutMigrationCommand) (Outcome, error) {
	if cmd.ManifestHash == "" {
		return Outcome{}, model.InvalidInputFault("the migration requires the exact preview manifest hash")
	}
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	state := view.State
	if state.Workflow.ID != wf || (state.Workflow.LayoutVersion != 1 && state.Workflow.LayoutVersion != 2) {
		return Outcome{}, model.InvalidInputFault("the workflow does not carry a supported migration layout")
	}
	record, err := st.LayoutMigration(ctx, wf)
	if err != nil {
		return Outcome{}, err
	}
	if record.ManifestHash != cmd.ManifestHash {
		return Outcome{}, model.NewFault(model.CodeApprovalInputChanged,
			"the execute confirmation does not match the persisted migration manifest")
	}
	intent, effectID, err := persistedMigrationIntent(view.PendingEffects, wf, record)
	if err != nil {
		return Outcome{}, err
	}
	manifest, err := readMigrationManifest(record.ManifestPath, record.ManifestHash)
	if err != nil {
		return Outcome{}, err
	}
	if !samePathMoves(intent.Moves, manifest.Moves) {
		return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the persisted layout migration intent does not match its immutable manifest")
	}

	// The ordered manifest plus source/destination/Git registry facts are
	// the recoverable progress ledger. A landed move is verified and
	// skipped; an unlanded move is verified and executed; ambiguous facts
	// stably block. Execute never derives a new move list from live paths.
	for i, mv := range intent.Moves {
		src, dst := pathPresent(mv.Source), pathPresent(mv.Destination)
		switch {
		case src && !dst:
			if err := a.verifyMigrationMove(ctx, mv, mv.Source); err != nil {
				return Outcome{}, err
			}
			if err := a.performMigrationMove(ctx, wf, mv); err != nil {
				return Outcome{}, err
			}
			if !pathPresent(mv.Destination) || pathPresent(mv.Source) {
				return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
					fmt.Sprintf("layout migration move %d did not settle to its destination", i))
			}
			if err := a.verifyMigrationMove(ctx, mv, mv.Destination); err != nil {
				return Outcome{}, err
			}
		case !src && dst:
			if err := a.verifyMigrationMove(ctx, mv, mv.Destination); err != nil {
				return Outcome{}, err
			}
		case src && dst:
			return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				fmt.Sprintf("layout migration move %d has both source and destination", i))
		default:
			return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				fmt.Sprintf("layout migration move %d has neither source nor destination", i))
		}
	}

	// A crash after the Layout transaction is recognized as completion;
	// only the matching migration/effect status markers remain to settle.
	if state.Workflow.LayoutVersion == 2 {
		if err := st.MarkLayoutMigrationCompleted(ctx, wf, record.ID, effectID); err != nil {
			return Outcome{}, err
		}
		return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime}, nil
	}
	// Advance the persisted Layout facts and mark the migration complete
	// in one transaction: layout_version=2, the Workspace Path and Branch
	// (the moved integration Worktree/Branch), and the verified Head.
	cd, err := st.Transact(ctx, view.AggregateVersion, func(s model.State) (model.Decision, error) {
		if s.Workflow.ID != wf || s.Workflow.LayoutVersion != 1 {
			return model.Decision{}, model.InvalidInputFault("the workflow is no longer on the legacy layout")
		}
		b := &decisionBuilder{}
		m := model.WorkflowMutation{
			ID: s.Workflow.ID, Project: s.Workflow.Project,
			Stage: s.Workflow.Stage, Runtime: s.Workflow.Runtime,
			TargetBranch: s.Workflow.TargetBranch, BaseCommit: s.Workflow.BaseCommit,
			IntegrationBranch:      s.Workflow.IntegrationBranch,
			IntegrationHead:        s.Workflow.IntegrationHead,
			LayoutVersion:          2,
			WorkspacePath:          a.layout.Workspace(wf),
			WorkspaceBranch:        s.Workflow.IntegrationBranch,
			VerifiedWorkspaceHead:  s.Workflow.IntegrationHead,
			CandidateWorkspaceHead: s.Workflow.IntegrationHead,
			CancelIntent:           s.Workflow.CancelIntent,
		}
		b.mutate(m)
		b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "legacy layout migrated to the aggregated workspace")
		return b.decision(), nil
	})
	if err != nil {
		return Outcome{}, err
	}
	_ = cd
	if err := st.MarkLayoutMigrationCompleted(ctx, wf, record.ID, effectID); err != nil {
		return Outcome{}, err
	}
	return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime}, nil
}

func persistedMigrationIntent(pending []store.PendingEffect, wf model.WorkflowID, record store.LayoutMigrationRecord) (model.LayoutMigrationIntent, string, error) {
	for _, effect := range pending {
		intent, ok := effect.Intent.(model.LayoutMigrationIntent)
		if !ok {
			continue
		}
		if intent.Workflow == wf && intent.MigrationID == record.ID && intent.ManifestHash == record.ManifestHash {
			return intent, effect.ID, nil
		}
		return model.LayoutMigrationIntent{}, "", model.NewFault(model.CodeEvidenceSubjectChanged,
			"the pending layout migration intent does not match the persisted migration row")
	}
	if record.Status == "COMPLETED" {
		return model.LayoutMigrationIntent{}, "", model.InvalidInputFault("the layout migration is already complete")
	}
	return model.LayoutMigrationIntent{}, "", model.InvariantFault(fmt.Errorf("prepared layout migration has no recoverable intent"))
}

func readMigrationManifest(path, hash string) (layout.MigrationPreview, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration manifest cannot be read")
	}
	var manifest layout.MigrationPreview
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.Hash() != hash || manifest.ManifestHash != hash {
		return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration manifest failed hash validation")
	}
	return manifest, nil
}

func samePathMoves(a, b []model.PathMove) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// verifyMigrationMove binds either side of a worktree move to the exact
// registry path, branch and head from the immutable intent. Artifact
// moves are path-bound by the immutable manifest and no-overwrite facts.
func (a *Application) verifyMigrationMove(ctx context.Context, mv model.PathMove, at string) error {
	if mv.Kind == model.MoveKindArtifact {
		digest, err := layout.DigestPath(at)
		if err != nil || mv.Digest == "" || digest != mv.Digest {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"layout migration artifact identity drifted: "+at)
		}
		return nil
	}
	facts, err := a.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return model.NewFault(model.CodeEvidenceSubjectChanged, "the Git worktree registry is unreadable")
	}
	list, ok := facts.(gitflow.WorktreeFacts)
	if !ok {
		return model.InvariantFault(fmt.Errorf("worktree registry observation has an unexpected type"))
	}
	clean := filepath.Clean(at)
	for _, entry := range list.Entries {
		if filepath.Clean(entry.Path) != clean {
			continue
		}
		if entry.Head != mv.Head || entry.Branch != mv.Branch || (mv.Branch == "" && !entry.Detached) {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"layout migration worktree identity drifted: "+at)
		}
		return nil
	}
	return model.NewFault(model.CodeEvidenceSubjectChanged,
		"layout migration worktree is absent from the Git registry: "+at)
}

// verifyMigrationSources re-observes every Worktree source against its
// recorded branch/head identity before any move (compare-and-swap): a
// missing or drifted source blocks the migration closed with nothing
// moved. Artifact sources are verified to exist.
func (a *Application) verifyMigrationSources(ctx context.Context, moves []model.PathMove) error {
	for _, mv := range moves {
		switch mv.Kind {
		case model.MoveKindWorktree:
			facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: mv.Source})
			if err != nil {
				return model.NewFault(model.CodeEvidenceSubjectChanged,
					"migration source worktree is unreadable: "+mv.Source)
			}
			st, ok := facts.(gitflow.StatusFacts)
			if !ok {
				return model.InvariantFault(fmt.Errorf("migration worktree observation has an unexpected type"))
			}
			if mv.Head != "" && st.Head != mv.Head {
				return model.NewFault(model.CodeEvidenceSubjectChanged,
					"migration source worktree head drifted: "+mv.Source)
			}
			if mv.Branch != "" {
				rf, err := a.git.Observe(ctx, gitflow.RefLookup{Ref: "refs/heads/" + mv.Branch})
				if err != nil {
					return err
				}
				ref, ok := rf.(gitflow.RefFacts)
				if !ok {
					return model.InvariantFault(fmt.Errorf("migration branch observation has an unexpected type"))
				}
				if !ref.Exists {
					return model.NewFault(model.CodeEvidenceSubjectChanged,
						"migration source branch is missing: "+mv.Branch)
				}
			}
		case model.MoveKindArtifact:
			if _, err := os.Stat(mv.Source); err != nil {
				if os.IsNotExist(err) {
					// The artifacts root and the static workflow.yaml are
					// created lazily and may legitimately be absent on a
					// workflow that never ran a planning Session; nothing
					// to move for those.
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// performMigrationMove executes one move of the manifest: a managed
// `git worktree move` for a Worktree, or a safe path move for an
// Artifact directory/file. The destination parent is created 0700 through
// the security guard.
func (a *Application) performMigrationMove(ctx context.Context, wf model.WorkflowID, mv model.PathMove) error {
	if err := a.validateMigrationMovePaths(wf, mv); err != nil {
		return err
	}
	if err := a.ensureWorktreeParent(mv.Destination); err != nil {
		return err
	}
	switch mv.Kind {
	case model.MoveKindWorktree:
		res, err := a.git.Execute(ctx, gitflow.MoveWorktree{From: mv.Source, To: mv.Destination})
		if err != nil {
			return err
		}
		if _, ok := res.(gitflow.WorktreeMovedResult); !ok {
			return model.InvariantFault(fmt.Errorf("worktree move result has an unexpected type"))
		}
		return nil
	case model.MoveKindArtifact:
		return movePath(ctx, mv.Source, mv.Destination)
	default:
		return model.InvalidInputFault("unknown migration move kind " + string(mv.Kind))
	}
}

func (a *Application) validateMigrationMovePaths(wf model.WorkflowID, mv model.PathMove) error {
	legacyWorktrees := filepath.Join(a.home, "worktrees", a.project.Key, string(wf))
	legacyArtifacts := filepath.Join(a.home, "projects", a.project.Key, "workflows", string(wf))
	wantSourceRoot := legacyArtifacts
	if mv.Kind == model.MoveKindWorktree {
		wantSourceRoot = legacyWorktrees
	} else if mv.Kind != model.MoveKindArtifact {
		return model.InvalidInputFault("unknown migration move kind " + string(mv.Kind))
	}
	if !strictlyInside(mv.Source, wantSourceRoot) || !strictlyInside(mv.Destination, a.layout.WorkflowRoot(wf)) {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"layout migration move escapes its exact managed source or destination root")
	}
	if info, err := os.Lstat(mv.Source); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"layout migration source is a symbolic link")
	}
	return nil
}

func strictlyInside(path, root string) bool {
	if path == "" || root == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// movePath performs one safe path move under the managed home: the
// source must be inside the managed home and the destination must not
// exist. The moved content keeps its modes; the destination parent is
// created 0700.
func movePath(ctx context.Context, source, destination string) error {
	if source == "" || destination == "" {
		return model.InvalidInputFault("migration move requires exact paths")
	}
	if _, err := os.Stat(source); err != nil {
		if os.IsNotExist(err) {
			return nil // the source is already absent: the move is a no-op
		}
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"migration destination already exists: "+destination)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return os.Rename(source, destination)
}

// decisionBuilder is a minimal Decision builder for the migration's
// final transaction (the migration is an Application-owned explicit
// operation, not a Kernel command).
type decisionBuilder struct {
	d model.Decision
}

func (b *decisionBuilder) mutate(m model.Mutation) {
	b.d.Mutations = append(b.d.Mutations, m)
}

func (b *decisionBuilder) event(kind model.EventKind, node model.NodeID, attempt model.AttemptKey, code model.Code, text string) {
	b.d.Events = append(b.d.Events, model.Event{
		Kind: kind, Workflow: "", Node: node, Attempt: attempt, Code: code, Text: text,
	})
}

func (b *decisionBuilder) decision() model.Decision { return b.d }
