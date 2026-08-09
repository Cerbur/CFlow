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
	"crypto/sha256"
	"database/sql"
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
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// migrationPreview derives the full Legacy Layout Migration Preview of
// one workflow. Persisted Agent Task Nodes and observed legacy task
// directories are unioned by Node ID; every task appears exactly once and
// is bound to its exact Git registry branch and HEAD.
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
	nameSet := map[string]struct{}{}
	for id, node := range state.Nodes {
		if node != nil && node.Kind == model.NodeAgentTask {
			nameSet[string(id)] = struct{}{}
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nameSet[e.Name()] = struct{}{}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	// layout.Preview includes persisted-node task moves without observable
	// HEADs. Replace that inventory with the authoritative union below.
	baseMoves := preview.Moves[:0]
	for _, move := range preview.Moves {
		if move.Kind == model.MoveKindWorktree && strictlyInside(move.Source, legacyTasks) {
			continue
		}
		baseMoves = append(baseMoves, move)
	}
	preview.Moves = baseMoves
	registryFacts, err := a.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return layout.MigrationPreview{}, err
	}
	registry := map[string]gitflow.WorktreeEntry{}
	for _, entry := range registryFacts.(gitflow.WorktreeFacts).Entries {
		registry[filepath.Clean(entry.Path)] = entry
	}
	for _, name := range names {
		node := model.NodeID(name)
		source := filepath.Join(legacyTasks, name)
		entry, ok := registry[filepath.Clean(source)]
		if !ok || entry.Detached || entry.Head == "" {
			return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"legacy task worktree is absent or detached in the Git registry")
		}
		expectedBranch := "cflow/" + string(wf) + "/task-" + name
		if persisted := state.Nodes[node]; persisted != nil && persisted.Branch != "" {
			expectedBranch = persisted.Branch
		}
		if entry.Branch != expectedBranch {
			return layout.MigrationPreview{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"legacy task worktree branch drifted")
		}
		preview.Moves = append(preview.Moves, model.PathMove{
			Kind:        model.MoveKindWorktree,
			Source:      source,
			Destination: a.layout.Task(wf, node),
			Branch:      entry.Branch,
			Head:        entry.Head,
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
			if view.LayoutMigration == nil || view.LayoutMigration.ID != intent.MigrationID ||
				view.LayoutMigration.Status != "PREPARED" || view.LayoutMigration.ManifestHash != intent.ManifestHash {
				return nil, model.NewFault(model.CodeEvidenceSubjectChanged,
					"the authoritative layout migration row does not match its pending intent")
			}
			manifest, err := readMigrationManifest(view.LayoutMigration.ManifestPath, view.LayoutMigration.ManifestHash)
			expectedManifestPath := filepath.Join(a.layout.StateDir(wf), "layout-migrations", intent.MigrationID+".json")
			if err != nil || filepath.Clean(view.LayoutMigration.ManifestPath) != filepath.Clean(expectedManifestPath) ||
				manifest.MigrationID != intent.MigrationID || manifest.Workflow != wf ||
				manifest.DatabaseImpact.WorkspacePath != a.layout.Workspace(wf) ||
				manifest.DatabaseImpact.WorkspaceBranch != view.State.Workflow.IntegrationBranch ||
				manifest.DatabaseImpact.WorkspaceHead != view.State.Workflow.IntegrationHead ||
				manifest.SourceSnapshot.AggregateVersion+1 != uint64(view.AggregateVersion) ||
				manifest.SourceSnapshot.LayoutVersion != 1 ||
				!samePathMoves(manifest.Moves, intent.Moves) {
				return nil, model.NewFault(model.CodeEvidenceSubjectChanged,
					"the immutable layout migration manifest does not match its pending intent")
			}
			return MigrationPreviewView{
				Workflow: wf, From: 1, To: 2, Moves: append([]model.PathMove(nil), intent.Moves...),
				ManifestHash: intent.ManifestHash, MigrationID: intent.MigrationID, Status: "PREPARED",
				ManifestPath: view.LayoutMigration.ManifestPath,
				BackupPath:   manifest.Backup.Path, BackupHash: manifest.Backup.SHA256, BackupSize: manifest.Backup.Size,
				SourceSnapshotHash:      manifest.SourceSnapshotHash,
				ExpectedWorkspacePath:   manifest.DatabaseImpact.WorkspacePath,
				ExpectedWorkspaceBranch: manifest.DatabaseImpact.WorkspaceBranch,
				ExpectedWorkspaceHead:   manifest.DatabaseImpact.WorkspaceHead,
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
		ExpectedWorkspacePath:   a.layout.Workspace(wf),
		ExpectedWorkspaceBranch: view.State.Workflow.IntegrationBranch,
		ExpectedWorkspaceHead:   view.State.Workflow.IntegrationHead,
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
	// Preflight the complete manifest before persisting evidence. Prepare
	// only accepts an entirely pending sequence; no destination may exist.
	firstPending, err := a.preflightMigrationMoves(ctx, preview.Moves, false)
	if err != nil || firstPending != 0 {
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"layout migration Prepare requires every move to be pending")
	}
	id := migrationID(wf, preview.ManifestHash)
	manifestPath := filepath.Join(a.layout.StateDir(wf), "layout-migrations", id+".json")
	if err := a.ensureWorktreeParent(manifestPath); err != nil {
		return Outcome{}, err
	}
	backupPath := strings.TrimSuffix(manifestPath, ".json") + ".db.backup"
	backup, err := st.BackupLayoutMigration(ctx, backupPath)
	if err != nil {
		return Outcome{}, err
	}
	// The source snapshot records the aggregate version the migration was
	// prepared against, excluding the migration intent's own commit. On an
	// idempotent retry the intent is already pending (its commit advanced
	// the version), so subtract one to reproduce the identical immutable
	// manifest bytes the first Prepare persisted.
	snapshotVersion := uint64(view.AggregateVersion)
	for _, pending := range view.PendingEffects {
		if existing, ok := pending.Intent.(model.LayoutMigrationIntent); ok && existing.MigrationID == id {
			snapshotVersion = uint64(view.AggregateVersion) - 1
		}
	}
	snapshot := layout.SourceSnapshot{
		AggregateVersion: snapshotVersion, LayoutVersion: state.Workflow.LayoutVersion,
		IntegrationBranch: state.Workflow.IntegrationBranch, IntegrationHead: state.Workflow.IntegrationHead,
		BaseCommit: state.Workflow.BaseCommit, PreviewHash: preview.ManifestHash,
	}
	snapshotBody, _ := json.Marshal(snapshot)
	snapshotSum := sha256.Sum256(snapshotBody)
	manifest := layout.MigrationManifest{
		MigrationID: id, Workflow: wf, PreviewHash: preview.ManifestHash,
		From: preview.From, To: preview.To, Moves: append([]model.PathMove(nil), preview.Moves...),
		Backup:         layout.BackupEvidence{Path: backup.Path, SHA256: backup.SHA256, Size: backup.Size},
		SourceSnapshot: snapshot, SourceSnapshotHash: fmt.Sprintf("%x", snapshotSum[:]),
		DatabaseImpact: layout.DatabaseImpact{FromLayoutVersion: 1, ToLayoutVersion: 2,
			WorkspacePath: a.layout.Workspace(wf), WorkspaceBranch: state.Workflow.IntegrationBranch,
			WorkspaceHead: state.Workflow.IntegrationHead},
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		return Outcome{}, model.InvariantFault(fmt.Errorf("migration manifest cannot be serialized"))
	}
	manifestSum := sha256.Sum256(manifestBody)
	manifestHash := fmt.Sprintf("%x", manifestSum[:])
	if err := a.writeMigrationManifest(manifestBody, manifestPath); err != nil {
		return Outcome{}, err
	}
	if err := st.RecordLayoutMigration(ctx, wf, id, manifestPath, manifestHash); err != nil {
		return Outcome{}, err
	}
	intent := model.LayoutMigrationIntent{
		MigrationID: id, Workflow: wf, ManifestHash: manifestHash, PreviewHash: preview.ManifestHash,
		Moves: append([]model.PathMove(nil), preview.Moves...),
	}
	for _, pending := range view.PendingEffects {
		if existing, ok := pending.Intent.(model.LayoutMigrationIntent); ok {
			if existing.MigrationID == id && existing.ManifestHash == manifestHash {
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

// writeMigrationManifest persists the immutable migration manifest (the
// full MigrationManifest bytes, whose SHA-256 is the recorded manifest
// hash) under the workflow's state directory through the security guard,
// so the Execute step and the Recovery engine read the exact bound moves.
func (a *Application) writeMigrationManifest(data []byte, path string) error {
	if _, err := os.Lstat(path); err == nil {
		// The manifest already exists (a crash mid-Prepare left it on
		// disk): it must be a regular managed file, and its bytes must be
		// identical — the manifest is immutable. A symlink or a different
		// content fails closed.
		if _, checkErr := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindFile}); checkErr != nil {
			return checkErr
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || string(existing) != string(data) {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"the immutable layout migration manifest already exists with different content")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be persisted"))
	}
	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		return model.InvariantFault(fmt.Errorf("migration manifest cannot be persisted"))
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
	return syncManagedFileAndDir(path)
}

func syncManagedFileAndDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// executeMigration performs the ordered moves of the bound manifest and
// advances the persisted Layout facts to Version 2. It preflights the
// entire immutable manifest before the first side effect and reconstructs
// progress only as a landed prefix followed by a pending suffix.
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
	intent, effectID, err := persistedMigrationIntent(view.PendingEffects, wf, record)
	if err != nil {
		return Outcome{}, err
	}
	if cmd.ManifestHash != record.ManifestHash && cmd.ManifestHash != intent.PreviewHash {
		return Outcome{}, model.NewFault(model.CodeApprovalInputChanged,
			"the execute confirmation does not match the persisted migration manifest")
	}
	manifest, err := readMigrationManifest(record.ManifestPath, record.ManifestHash)
	if err != nil {
		return Outcome{}, err
	}
	expectedManifestPath := filepath.Join(a.layout.StateDir(wf), "layout-migrations", record.ID+".json")
	if record.Status != "PREPARED" || filepath.Clean(record.ManifestPath) != filepath.Clean(expectedManifestPath) ||
		manifest.MigrationID != record.ID || manifest.Workflow != wf ||
		manifest.DatabaseImpact.WorkspacePath != a.layout.Workspace(wf) ||
		manifest.DatabaseImpact.WorkspaceBranch != state.Workflow.IntegrationBranch ||
		manifest.DatabaseImpact.WorkspaceHead != state.Workflow.IntegrationHead ||
		!samePathMoves(intent.Moves, manifest.Moves) {
		return Outcome{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the persisted layout migration intent does not match its immutable manifest")
	}

	// Preflight the complete immutable manifest first: only an exactly
	// landed prefix followed by a pending suffix may proceed, and the DB
	// may not have advanced while any move is still pending.
	firstPending, err := a.preflightMigrationMoves(ctx, intent.Moves, state.Workflow.LayoutVersion == 2)
	if err != nil {
		return Outcome{}, err
	}
	// Only after the complete manifest preflight proves a landed prefix
	// followed by a pending suffix may the first pending move execute.
	for i := firstPending; i < len(intent.Moves); i++ {
		mv := intent.Moves[i]
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
			WorkspacePath:          manifest.DatabaseImpact.WorkspacePath,
			WorkspaceBranch:        manifest.DatabaseImpact.WorkspaceBranch,
			VerifiedWorkspaceHead:  manifest.DatabaseImpact.WorkspaceHead,
			CandidateWorkspaceHead: manifest.DatabaseImpact.WorkspaceHead,
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

func (a *Application) preflightMigrationMoves(ctx context.Context, moves []model.PathMove, dbAdvanced bool) (int, error) {
	firstPending := len(moves)
	pendingSeen := false
	for i, mv := range moves {
		src, dst := pathPresent(mv.Source), pathPresent(mv.Destination)
		switch {
		case src && !dst:
			if dbAdvanced {
				return 0, model.NewFault(model.CodeEvidenceSubjectChanged,
					fmt.Sprintf("database advanced before migration move %d landed", i))
			}
			pendingSeen = true
			if firstPending == len(moves) {
				firstPending = i
			}
			if err := a.verifyMigrationMove(ctx, mv, mv.Source); err != nil {
				return 0, err
			}
		case !src && dst:
			if pendingSeen {
				return 0, model.NewFault(model.CodeEvidenceSubjectChanged,
					fmt.Sprintf("layout migration move %d landed out of order", i))
			}
			if err := a.verifyMigrationMove(ctx, mv, mv.Destination); err != nil {
				return 0, err
			}
		case src && dst:
			return 0, model.NewFault(model.CodeEvidenceSubjectChanged,
				fmt.Sprintf("layout migration move %d has both source and destination", i))
		default:
			return 0, model.NewFault(model.CodeEvidenceSubjectChanged,
				fmt.Sprintf("layout migration move %d has neither source nor destination", i))
		}
	}
	return firstPending, nil
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

func readMigrationManifest(path, hash string) (layout.MigrationManifest, error) {
	if _, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindFile}); err != nil {
		return layout.MigrationManifest{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration manifest cannot be read")
	}
	sum := sha256.Sum256(body)
	var manifest layout.MigrationManifest
	if err := json.Unmarshal(body, &manifest); err != nil || fmt.Sprintf("%x", sum[:]) != hash {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration manifest failed hash validation")
	}
	preview := manifest.Preview()
	if manifest.MigrationID == "" || manifest.Workflow == "" || manifest.PreviewHash == "" ||
		preview.Hash() != manifest.PreviewHash || manifest.SourceSnapshotHash == "" ||
		manifest.DatabaseImpact.FromLayoutVersion != 1 || manifest.DatabaseImpact.ToLayoutVersion != 2 ||
		manifest.DatabaseImpact.WorkspacePath == "" || manifest.DatabaseImpact.WorkspaceBranch == "" ||
		manifest.DatabaseImpact.WorkspaceHead == "" {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration manifest is incomplete")
	}
	snapshotBody, _ := json.Marshal(manifest.SourceSnapshot)
	snapshotSum := sha256.Sum256(snapshotBody)
	if fmt.Sprintf("%x", snapshotSum[:]) != manifest.SourceSnapshotHash ||
		manifest.SourceSnapshot.PreviewHash != manifest.PreviewHash {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the layout migration source snapshot identity is invalid")
	}
	expectedBackup := strings.TrimSuffix(path, ".json") + ".db.backup"
	if manifest.Backup.Path != expectedBackup || manifest.Backup.SHA256 == "" || manifest.Backup.Size <= 0 {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the layout migration backup evidence is incomplete")
	}
	if _, err := security.CheckPath(security.PathRequest{Path: manifest.Backup.Path, Kind: security.KindFile}); err != nil {
		return layout.MigrationManifest{}, err
	}
	backupBody, err := os.ReadFile(manifest.Backup.Path)
	if err != nil {
		return layout.MigrationManifest{}, err
	}
	backupSum := sha256.Sum256(backupBody)
	if int64(len(backupBody)) != manifest.Backup.Size || fmt.Sprintf("%x", backupSum[:]) != manifest.Backup.SHA256 {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration backup identity drifted")
	}
	db, err := sql.Open("sqlite", "file:"+manifest.Backup.Path+"?mode=ro")
	if err != nil {
		return layout.MigrationManifest{}, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return layout.MigrationManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration backup failed integrity verification")
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
// exist. The move uses an atomic no-replace primitive so a destination
// created after the preflight is never overwritten. The moved content
// keeps its modes; the destination parent is created 0700.
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
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := platform.AtomicRenameNoReplace(source, destination); err != nil {
		if os.IsExist(err) {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"migration destination already exists: "+destination)
		}
		return err
	}
	return nil
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
