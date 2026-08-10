package app

// Freeze Discussion Change Set (TUI task 5): one immutable, fully
// Runtime-generated snapshot of a Workflow's candidate Change Set at a
// discussion Session turn. The Change Set is never Agent-authored; every
// field comes from the Git facts the Runtime observes in the Workflow's
// long-lived Workspace, and the body structure is fixed by the model.Go
// types, canonical JSON, and the artifact Hash.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// executeFreeze freezes the candidate Change Set of the Workflow's
// Workspace at the bound discussion Session turn (TUI task 5): the
// Runtime observes the exact Git facts (Base/Heads, committed Commit
// Range, tracked Diff, Untracked inventory, Dirty Fingerprint), arranges
// them into the typed, canonical ChangeSet body, and writes the next
// immutable ArtifactChangeSet Revision. The Change Set is never
// Agent-authored; freezing again after further turns produces the next
// Revision while every earlier Revision stays byte-identical.
func (a *Application) executeFreeze(ctx context.Context, st *store.Store, wf model.WorkflowID, cmd FreezeDiscussionCommand) (Outcome, error) {
	if !cmd.Session.Valid() {
		return Outcome{}, model.InvalidInputFault("freezing the change set requires a discussion session identity")
	}
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	if view.State.Workflow.ID != wf {
		return Outcome{}, model.InvalidInputFault("the workflow does not exist")
	}
	base := view.State.Workflow.BaseCommit
	if base == "" {
		return Outcome{}, model.InvariantFault(fmt.Errorf("the workflow has no recorded base commit"))
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return Outcome{}, err
	}
	status, err := a.observeChangeSetStatus(ctx, cwd)
	if err != nil {
		return Outcome{}, err
	}
	rangeFacts, err := a.observeCommitRange(ctx, base, status.Head)
	if err != nil {
		return Outcome{}, err
	}
	body, err := assembleChangeSet(base, cwd, status, rangeFacts, string(cmd.Session))
	if err != nil {
		return Outcome{}, err
	}
	ref, err := a.freezeChangeSet(ctx, wf, cmd.Session, body)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{
		Workflow: wf,
		Stage:    view.State.Workflow.Stage,
		Runtime:  view.State.Workflow.Runtime,
		// ChangeSet is the Change Set this freeze wrote.
		ChangeSet: &ChangeSetView{
			Ref:         ref,
			Base:        base,
			Candidate:   status.Head,
			Verified:    status.Head,
			Fingerprint: status.Dirty.Combined,
			Dirty:       !status.Clean(),
		},
	}, nil
}

// observeChangeSetStatus observes the porcelain-v2 status and the Dirty
// Fingerprint of the candidate Workspace over the exact Git-visible
// working tree.
func (a *Application) observeChangeSetStatus(ctx context.Context, cwd string) (gitflow.StatusFacts, error) {
	if a.git == nil {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("the git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: cwd, UntrackedAll: true})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	status, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("the workspace status observation has an unexpected type"))
	}
	if status.Head == "" {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("the workspace has no head; no candidate change set can be frozen"))
	}
	return status, nil
}

// observeCommitRange returns the committed Commit Range and changed paths
// of the candidate from the recorded Base to the Workspace Head (empty
// when the Workspace has not moved past the Base).
func (a *Application) observeCommitRange(ctx context.Context, base, head string) (gitflow.RangeFacts, error) {
	if base == head {
		return gitflow.RangeFacts{From: base, To: head}, nil
	}
	facts, err := a.git.Observe(ctx, gitflow.HistoryRange{From: base, To: head})
	if err != nil {
		return gitflow.RangeFacts{}, err
	}
	rangeFacts, ok := facts.(gitflow.RangeFacts)
	if !ok {
		return gitflow.RangeFacts{}, model.InvariantFault(fmt.Errorf("the commit range observation has an unexpected type"))
	}
	return rangeFacts, nil
}

// assembleChangeSet arranges the observed Git facts into the canonical
// ArtifactChangeSet payload of one frozen Revision. The body structure is
// fixed by the model.ChangeSet Go types; the returned payload includes the
// canonical content hash of the body.
func assembleChangeSet(base, cwd string, status gitflow.StatusFacts, rangeFacts gitflow.RangeFacts, sessionID string) ([]byte, error) {
	cs := model.ChangeSet{
		BaseCommit:       base,
		CandidateHead:    status.Head,
		VerifiedHead:     status.Head,
		Commits:          append([]string(nil), rangeFacts.Commits...),
		TrackedDiff:      trackTracked(cwd, rangeFacts, status),
		Untracked:        inventoryUntracked(status),
		DirtyFingerprint: status.Dirty.Combined,
		SessionID:        sessionID,
	}
	return marshalChangeSet(cs)
}

// marshalChangeSet canonically serializes one ChangeSet body: the content
// hash is computed over the body without its own content_hash field, then
// the self-reference is embedded and the final payload is serialized with
// Go's deterministic struct field ordering (the same canonical JSON the
// Artifact Store hashes).
func marshalChangeSet(cs model.ChangeSet) ([]byte, error) {
	cs.ContentHash = ""
	sum := sha256.Sum256(canonicalChangeSetJSON(cs))
	cs.ContentHash = hex.EncodeToString(sum[:])
	return canonicalChangeSetJSON(cs), nil
}

// canonicalChangeSetJSON returns the deterministic JSON serialization of
// one model.ChangeSet (struct field order, no injected whitespace).
func canonicalChangeSetJSON(cs model.ChangeSet) []byte {
	data, err := json.Marshal(cs)
	if err != nil {
		return nil
	}
	return data
}

// trackTracked inventories every tracked path a candidate changed relative
// to the Base: the committed paths of the Commit Range plus every
// staged/unstaged path, each with its identity facts (path, rename
// source, mode, working-tree size, index hash, and porcelain status).
func trackTracked(cwd string, rangeFacts gitflow.RangeFacts, status gitflow.StatusFacts) []model.ChangeSetEntry {
	var tracked []gitflow.PathEntry
	for _, path := range rangeFacts.ChangedPaths {
		tracked = append(tracked, gitflow.PathEntry{Path: path})
	}
	tracked = append(tracked, status.Staged...)
	tracked = append(tracked, status.Unstaged...)

	seen := map[string]bool{}
	entries := []model.ChangeSetEntry{}
	for _, p := range tracked {
		if p.Path == "" || seen[p.Path] {
			continue
		}
		seen[p.Path] = true
		size := int64(0)
		if s, err := os.Stat(filepath.Join(cwd, p.Path)); err == nil {
			size = s.Size()
		}
		statusCode := ""
		if p.X != 0 || p.Y != 0 {
			statusCode = string([]byte{p.X, p.Y})
		}
		entries = append(entries, model.ChangeSetEntry{
			Path:     p.Path,
			Original: p.Original,
			Mode:     p.Mode,
			Size:     size,
			Hash:     p.Hash,
			Status:   statusCode,
		})
	}
	return entries
}

// inventoryUntracked inventories every Untracked path with its identity
// facts (Git status, working-tree size).
func inventoryUntracked(status gitflow.StatusFacts) []model.ChangeSetEntry {
	entries := make([]model.ChangeSetEntry, 0, len(status.Untracked))
	for _, p := range status.Untracked {
		size := int64(0)
		if s, err := os.Stat(filepath.Join(status.Dir, p.Path)); err == nil {
			size = s.Size()
		}
		entries = append(entries, model.ChangeSetEntry{
			Path:   p.Path,
			Size:   size,
			Status: string([]byte{p.X, p.Y}),
		})
	}
	return entries
}

// freezeChangeSet writes the immutable next ArtifactChangeSet Revision
// through the redaction boundary of the Artifact Store. The Session is
// bound through the artifact Producer lineage.
func (a *Application) freezeChangeSet(ctx context.Context, wf model.WorkflowID, session model.SessionID, body []byte) (model.ArtifactRef, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	next := 1
	if ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactChangeSet}); err == nil {
		next = ref.Revision + 1
	}
	ref, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactChangeSet,
		Revision:      next,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: "change-set", SessionID: string(session)},
		Body:          body,
	})
	if err != nil {
		return model.ArtifactRef{}, err
	}
	return ref, nil
}
