package app

// The deterministic apply verification and the independent Apply
// Verification Session (Task 19, PRD 已确认：显式受保护 Apply step 4): the
// approved Catalog Revision is revalidated (Revision/hash, the wrapper
// source hash, and the PATH executable absolute path/binary hash), the
// apply_verify command runs inside the Apply Worktree, and the
// independent Session reviews the combined Target + Integration result.
// Same-package split of the Application seam: no public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/verify"
)

// applyVerificationRun runs the full deterministic apply verification on
// the combined result inside the Apply Worktree (PRD step 4): the
// approved Catalog Revision is revalidated (Revision/hash), the
// apply_verify command entry is selected, and the wrapper source hash /
// PATH executable absolute path and binary hash are re-verified in the
// Apply Worktree. The Evidence Manifest is persisted at the deterministic
// evidence path.
func (a *Application) applyVerificationRun(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, stagingHead, path string) (model.EvidenceManifest, error) {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	catalog, err := a.applyCatalogRef(ctx, wf, att, view.State)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	validated, err := engine.ValidateCatalog(ctx, catalog)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	commandID := ""
	for id, entry := range validated.Entries {
		if entry.Purpose == string(verify.PurposeApplyVerify) {
			commandID = id
			break
		}
	}
	if commandID == "" {
		return model.EvidenceManifest{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the approved catalog carries no apply_verify command")
	}
	manifest, err := engine.Run(ctx, verify.VerificationRequest{
		Node:        model.NodeID(string(att.ID)),
		Catalog:     catalog,
		CommandID:   commandID,
		Purpose:     verify.PurposeApplyVerify,
		Worktree:    path,
		CommitRange: att.TargetHead + ".." + stagingHead,
	})
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	if err := a.writeVerificationManifest(wf, model.NodeID(string(att.ID)), manifest); err != nil {
		return model.EvidenceManifest{}, err
	}
	return manifest, nil
}

// applyCatalogRef resolves the Catalog Revision the Apply verification
// must revalidate: the latest append-only APPLY_CATALOG approval bound to
// this attempt, else the Execution Approval's Catalog (PRD 已确认：Apply
// Command Identity Drift).
func (a *Application) applyCatalogRef(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, st model.State) (model.CatalogRef, error) {
	for i := len(st.Approvals) - 1; i >= 0; i-- {
		ap := st.Approvals[i]
		if ap.Kind != model.ApprovalApplyCatalog {
			continue
		}
		var ctxMap map[string]string
		if err := json.Unmarshal([]byte(ap.DecisionContext), &ctxMap); err != nil {
			continue
		}
		if ctxMap["attempt"] != string(att.ID) {
			continue
		}
		for _, ref := range ap.Refs {
			if ref.Type == model.ArtifactCatalog {
				return model.CatalogRef{Revision: ref.Revision, Hash: ref.Hash}, nil
			}
		}
	}
	if st.Workflow.ExecutionFacts == nil || st.Workflow.ExecutionFacts.CatalogRevision < 1 || st.Workflow.ExecutionFacts.CatalogHash == "" {
		return model.CatalogRef{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the approved catalog facts are missing")
	}
	return model.CatalogRef{Revision: st.Workflow.ExecutionFacts.CatalogRevision, Hash: st.Workflow.ExecutionFacts.CatalogHash}, nil
}

// applyIdentityDrifted reports whether the tree the Apply verification
// runs in no longer matches the approved Catalog's pinned wrapper
// identities (PRD 已确认：Apply Command Identity Drift): the revalidation
// compares the pinned wrapper hashes against the files of the attempt's
// Apply Worktree.
func (a *Application) applyIdentityDrifted(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, st model.State) bool {
	catalog, err := a.applyCatalogRef(ctx, wf, att, st)
	if err != nil {
		return false
	}
	if catalog.Hash != st.Workflow.ExecutionFacts.CatalogHash {
		// The approval already fixed a successor Catalog Revision.
		return false
	}
	path := a.applyWorktreePath(wf, att.Number)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return false
	}
	validated, err := engine.ValidateCatalog(ctx, catalog)
	if err != nil {
		return true // the approved identity no longer validates
	}
	for _, entry := range validated.Entries {
		if entry.ExecutableKind != verify.KindProjectRelative {
			continue
		}
		joined := filepath.Join(path, filepath.FromSlash(entry.Executable))
		data, err := os.ReadFile(joined)
		if err != nil || entry.SHA256 == "" || sha256HexString(data) != entry.SHA256 {
			return true
		}
	}
	return false
}

// rediscoverApplyCatalog re-discovers, validates, and fixes a NEW Apply
// Verification Catalog Revision from the Apply Worktree of the attempt
// (the tree the verification runs in, at the new Target HEAD) and writes
// it through the immutable Artifact Store as the successor Revision.
func (a *Application) rediscoverApplyCatalog(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt) (model.CatalogRef, error) {
	path := a.applyWorktreePath(wf, att.Number)
	wrappers, err := verify.DiscoverWrappers(path)
	if err != nil {
		return model.CatalogRef{}, err
	}
	pathExecs, err := verify.DiscoverPathExecutables()
	if err != nil {
		return model.CatalogRef{}, err
	}
	valid := make([]verify.Candidate, 0, len(wrappers)+len(pathExecs))
	for _, c := range append(wrappers, pathExecs...) {
		if err := verify.ValidateCandidate(c); err == nil {
			valid = append(valid, c)
		}
	}
	artStore, err := a.artifactStore(wf)
	if err != nil {
		return model.CatalogRef{}, err
	}
	revision := 1
	if latest, err := artStore.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog}); err == nil {
		revision = latest.Revision + 1
	}
	body, err := verify.CatalogBody(revision, valid)
	if err != nil {
		return model.CatalogRef{}, err
	}
	ref, err := artStore.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactCatalog,
		Revision:      revision,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		Producer:      artifact.ProducerRef{Purpose: "apply-catalog"},
		Body:          body,
	})
	if err != nil {
		return model.CatalogRef{}, err
	}
	return model.CatalogRef{Revision: ref.Revision, Hash: ref.Hash}, nil
}

// applyReviewProviderStart runs the independent Apply Verification
// Session (PRD step 4: 创建独立 Apply Verification Session 执行语义
// Review): a non-coding Session inside the Apply Worktree, bound to the
// exact refs and the deterministic apply verification manifest. The
// Worktree's HEAD and Git-visible state must be unchanged
// (UNEXPECTED_AGENT_MUTATION otherwise); the result echoes the
// deterministic manifest hash so the Kernel records the deterministic
// test-result evidence with the review pass.
func (a *Application) applyReviewProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.promptForPurpose(model.PurposeApplyVerification)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the apply verification purpose")
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	att := applyAttemptStagingOf(view.State)
	if att == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("no staging apply attempt for session %s", intent.Session))
	}
	cwd := a.applyWorktreePath(wf, att.Number)
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.applyReviewSessionInput(ctx, wf, att)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   intent.Purpose,
		Provider:  intent.Route,
		Prompt:    renderPrompt(prompt.Body, input),
		Input:     a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:       cwd,
		SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if res.Terminal != nil {
		out.Body = []byte(res.Terminal.Result)
	}
	out.ManifestHash = a.applyVerificationManifestHash(wf, att)
	return out, nil
}

// applyReviewSessionInput builds the Apply Verification Session's typed
// input block: the Target Branch and recorded Target HEAD, the
// Integration HEAD, the verified staging head, the approved Catalog
// Revision, and the deterministic apply verification manifest.
func (a *Application) applyReviewSessionInput(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt) (any, error) {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return nil, err
	}
	catalog, err := a.applyCatalogRef(ctx, wf, att, view.State)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"workflow":          string(wf),
		"target_branch":     view.State.Workflow.TargetBranch,
		"target_head":       att.TargetHead,
		"integration_head":  att.IntegrationHead,
		"staging_head":      att.StagingHead,
		"apply_branch":      a.applyBranchName(wf, att.Number),
		"catalog_revision":  catalog.Revision,
		"catalog_hash":      catalog.Hash,
		"verification_hash": a.applyVerificationManifestHash(wf, att),
		"message":           "review the combined target + integration result inside the apply worktree",
	}, nil
}

// applyVerificationManifestHash re-reads the persisted deterministic
// apply verification manifest and returns its self-hash ("" when
// absent).
func (a *Application) applyVerificationManifestHash(wf model.WorkflowID, att *model.ApplyAttempt) string {
	body, err := a.readVerificationManifestFile(wf, model.NodeID(string(att.ID)))
	if err != nil || len(body) == 0 {
		return ""
	}
	var m model.EvidenceManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Hash
}
