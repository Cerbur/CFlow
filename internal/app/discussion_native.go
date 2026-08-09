package app

// Native requirement discussion (design §9, TUI task 12): the TUI runs
// one Provider's native interactive resume of an exact CFlow Session in
// the Workflow's Workspace through the Bridge, then the Return Page
// offers Continue/Finish/Switch/Pause/Cancel. A managed Provider
// bootstrap establishes the Provider's OWN session identity (never a
// CFlow Session id); the Bridge return persists the process exit facts
// and moves the Session to INTERACTIVE_IDLE; Finish drives a managed
// structured resume on the same Provider Session that produces the
// immutable, schema-validated ArtifactDiscussionHandoff — the only
// discussion input Plan generation consumes.

import (
	"context"
	"encoding/json"
	"fmt"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/store"
)

// prepareNativeDiscussion is the prepare-case of PrepareNativeDiscussionCommand:
// it validates the workflow and the approved route, allocates the fresh
// interactive Session identity and its managed Process identity, and
// returns the kernel input that records the Session STARTING and requests
// the managed bootstrap effect (which binds the Provider's own session id).
func (a *Application) prepareNativeDiscussion(ctx context.Context, c PrepareNativeDiscussionCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if c.Provider == "" || len(c.Provider) > 128 {
		return nil, "", model.InvalidInputFault("a discussion provider is required and bounded")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf {
		return nil, "", model.InvalidInputFault("the workflow does not exist")
	}
	if view.State.Workflow.Stage != model.StageRequirementDiscussion {
		return nil, "", model.InvalidInputFault("native discussion requires the REQUIREMENT_DISCUSSION stage")
	}
	return model.PrepareNativeDiscussionInput{
		Provider: c.Provider,
		Session:  model.SessionID(a.ids(model.IDSession)),
		Process:  model.ProcessID(a.ids(model.IDProcess)),
	}, wf, nil
}

// prepareContinueNativeDiscussion is the prepare-case of
// ContinueNativeDiscussionCommand: the same Session is re-armed for
// another interactive turn on the SAME Provider Session and SAME provider
// binding (design §9.2). A lost or foreign Session fails closed.
func (a *Application) prepareContinueNativeDiscussion(ctx context.Context, c ContinueNativeDiscussionCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if !c.Session.Valid() {
		return nil, "", model.InvalidInputFault("continuing a native discussion requires the bound session identity")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	s := findSessionState(view.State, c.Session)
	if s == nil {
		return nil, "", model.InvalidInputFault("the discussion session is not bound to this workflow")
	}
	if s.ProviderSessionID == "" {
		return nil, "", model.NewFault(model.CodeProviderSessionIDMissing,
			"the discussion session has no bound provider session")
	}
	return model.ContinueNativeDiscussionInput{
		Session: c.Session,
		Process: model.ProcessID(a.ids(model.IDProcess)),
	}, wf, nil
}

// prepareSwitchAgent is the prepare-case of SwitchAgentCommand: a switch
// requires a DIFFERENT provider, creates the immutable redacted Context
// Bundle of the superseded Session, and allocates the NEW CFlow Session
// and its managed Process. The switch reason and the superseded Session
// linkage are persisted by the Kernel.
func (a *Application) prepareSwitchAgent(ctx context.Context, c SwitchAgentCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if c.Provider == "" || len(c.Provider) > 128 {
		return nil, "", model.InvalidInputFault("a switch provider is required and bounded")
	}
	if c.Reason == "" || len(c.Reason) > 4096 {
		return nil, "", model.InvalidInputFault("switching the discussion agent requires a bounded reason")
	}
	if !c.Session.Valid() {
		return nil, "", model.InvalidInputFault("switching the discussion agent requires the superseded session identity")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	s := findSessionState(view.State, c.Session)
	if s == nil {
		return nil, "", model.InvalidInputFault("the superseded discussion session is not bound to this workflow")
	}
	if c.Provider == s.Provider {
		return nil, "", model.NewFault(model.CodeSessionIndependenceViolation,
			"switching the discussion agent requires a different provider")
	}
	if s.ProviderSessionID == "" {
		return nil, "", model.NewFault(model.CodeProviderSessionIDMissing,
			"the superseded discussion session has no bound provider session")
	}
	// The immutable redacted Context Bundle of the superseded Session is
	// created and persisted through the same machinery the automatic
	// fallback uses (design 14.4); the switch reason rides in the bundle
	// Decisions so the successor Session's context is durable. The created
	// bundle reference is carried on the Kernel input: the Kernel persists
	// the reference on the new Session row and the managed bootstrap reads
	// the bundle content back from the evidence root, so the successor
	// Provider starts with the prior discussion context (design §9.4).
	rt, err := a.agentRuntime(ctx, view.State)
	if err != nil {
		return nil, "", err
	}
	if rt != nil {
		defer rt.Close()
	}
	var bundle agent.ContextBundle
	if rt != nil {
		ctxIn := a.resumeContext(ctx, wf, s.ID, s.Provider)
		ctxIn.Decisions = append([]string(nil), "switch-agent: "+c.Reason)
		bundle, err = rt.CreateContextBundle(ctx, agent.ContextBundleRequest{
			SessionID:         s.ID,
			ProviderSessionID: agent.ProviderSessionID(s.ProviderSessionID),
			Purpose:           s.Purpose,
			Context:           ctxIn,
		})
		if err != nil {
			return nil, "", err
		}
	}
	return model.SwitchAgentInput{
		Session:    model.SessionID(a.ids(model.IDSession)),
		Provider:   c.Provider,
		Reason:     c.Reason,
		Supersedes: c.Session,
		Process:    model.ProcessID(a.ids(model.IDProcess)),
		// The bundle reference the Kernel persists with the new Session.
		ContextBundleRevision: bundle.Revision,
		ContextBundlePath:     bundle.Path,
		ContextBundleSha256:   bundle.Hash,
	}, wf, nil
}

// prepareNativeDiscussionReturn is the prepare-case of
// NativeDiscussionReturnCommand: it revalidates the Session binding the
// Bridge ran on and the Workspace facts from the authoritative Git seams,
// then returns the kernel input that persists the process exit facts and
// moves the Session to INTERACTIVE_IDLE. A non-zero exit is NOT a
// discussion failure by itself.
func (a *Application) prepareNativeDiscussionReturn(ctx context.Context, c NativeDiscussionReturnCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if !c.Session.Valid() {
		return nil, "", model.InvalidInputFault("a native discussion return requires the bound session identity")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	s := findSessionState(view.State, c.Session)
	if s == nil {
		return nil, "", model.InvalidInputFault("the returned discussion session is not bound to this workflow")
	}
	// Revalidate the exact binding the Bridge ran on: the Provider and the
	// Provider's own session id must match the recorded facts (design 9.3).
	if c.Provider == "" || c.Provider != s.Provider {
		return nil, "", model.NewFault(model.CodeProviderBindingChanged,
			"the returned discussion provider does not match the recorded binding")
	}
	if c.ProviderSession == "" || string(c.ProviderSession) != s.ProviderSessionID {
		return nil, "", model.NewFault(model.CodeProviderBindingChanged,
			"the returned provider session does not match the recorded binding")
	}
	// Revalidate the Workspace facts: branch/HEAD, the dirty fingerprint,
	// and the Git in-progress state, all from the authoritative Git seams.
	workspaceHead, fingerprint, err := a.revalidateWorkspaceReturn(ctx, wf, s)
	if err != nil {
		return nil, "", err
	}
	// The managed Process record the prepare decision appended is the exact
	// process the interactive turn settles.
	processID := ""
	for _, p := range view.State.Processes {
		if p.Session == c.Session && p.Status == model.ProcessStatusRunning {
			processID = string(p.ID)
			break
		}
	}
	if processID == "" {
		return nil, "", model.InvalidInputFault("the returned discussion has no running managed process record")
	}
	return model.NativeDiscussionReturnInput{
		Session:                   c.Session,
		ExitCode:                  c.Exit.Code,
		ExitFact:                  exitFactName(c.Exit),
		Process:                   model.ProcessID(processID),
		Provider:                  c.Provider,
		ProviderSession:           string(c.ProviderSession),
		WorkspaceHead:             workspaceHead,
		WorkspaceDirtyFingerprint: fingerprint,
	}, wf, nil
}

// revalidateWorkspaceReturn observes the Workflow Workspace's authoritative
// Git facts on a native discussion return: the recorded Workspace Branch is
// re-verified against the Worktree registry, the observed HEAD and dirty
// fingerprint are compared against the recorded candidate facts when one
// exists, and an in-progress Git operation fails closed.
func (a *Application) revalidateWorkspaceReturn(ctx context.Context, wf model.WorkflowID, s *model.Session) (head, fingerprint string, err error) {
	if a.git == nil {
		return "", "", model.InvariantFault(fmt.Errorf("the git seam is not configured for this application"))
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return "", "", err
	}
	status, err := a.observeChangeSetStatus(ctx, cwd)
	if err != nil {
		return "", "", err
	}
	// The Worktree registry revalidates the recorded Workspace Branch and
	// the observed HEAD (design 9.3: revalidate the Workspace facts).
	reg, err := a.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return "", "", err
	}
	if wfacts, ok := reg.(gitflow.WorktreeFacts); ok {
		match := false
		for _, e := range wfacts.Entries {
			if e.Path != cwd {
				continue
			}
			if e.Branch == "" || e.Branch != a.workspaceBranch(ctx, wf) {
				return "", "", model.NewFault(model.CodeEvidenceSubjectChanged,
					"the workspace branch drifted from the recorded binding")
			}
			match = true
		}
		if !match {
			return "", "", model.NewFault(model.CodeEvidenceSubjectChanged,
				"the workspace is not registered as a worktree")
		}
	}
	// An unfinished Git operation (merge/rebase/... or an admin lock) makes
	// the return facts unsafe; fail closed.
	if facts, err := a.git.Observe(ctx, gitflow.WorktreeInProgress{Path: cwd}); err == nil {
		if p, ok := facts.(gitflow.WorktreeInProgressFacts); ok && (p.InProgress || p.Locked) {
			return "", "", model.NewFault(model.CodeDirtyWorktreeDrifted,
				"the workspace carries an in-progress git operation: "+p.Reason)
		}
	} else {
		return "", "", err
	}
	return status.Head, status.Dirty.Combined, nil
}

// workspaceBranch returns the recorded Workspace Branch of one aggregated
// workflow ("" for a legacy layout workflow).
func (a *Application) workspaceBranch(ctx context.Context, wf model.WorkflowID) string {
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return ""
	}
	return view.State.Workflow.WorkspaceBranch
}

// exitFactName renders the typed process exit fact of one interactive
// turn for the kernel record.
func exitFactName(e process.Exit) string {
	switch e.Fact {
	case process.FactProcessExit:
		return "process-exit"
	case process.FactSignaled:
		return "signaled"
	case process.FactTimeout:
		return "timeout"
	case process.FactCancelled:
		return "cancelled"
	}
	return "process-exit"
}

// queryDiscussionReturn projects the native discussion Return Page.
func (a *Application) queryDiscussionReturn(ctx context.Context, q DiscussionReturnQuery) (View, error) {
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
	dv := DiscussionReturnView{Workflow: wf}
	for i := len(view.State.Sessions) - 1; i >= 0; i-- {
		s := view.State.Sessions[i]
		if s.Purpose == model.PurposePlanning && s.Provider != "" {
			dv.Session = s.ID
			dv.Provider = s.Provider
			dv.ProviderSession = agent.ProviderSessionID(s.ProviderSessionID)
			dv.SessionStatus = s.Status
			break
		}
	}
	// The Return actions are legal only per the revalidated facts: a
	// resumable STARTING/INTERACTIVE_IDLE Session with a bound Provider
	// Session offers Continue/Finish/Switch/Pause/Cancel; a terminal
	// Session offers no discussion actions.
	if dv.Session != "" {
		switch dv.SessionStatus {
		case model.SessionStarting, model.SessionInteractiveIdle:
			if dv.ProviderSession != "" {
				dv.Actions = []string{"continue", "finish", "switch-agent", "pause", "cancel"}
			}
		}
	}
	// The frozen Change Set Ref (the latest revision), if one exists.
	if ref, err := a.latestChangeSetRef(ctx, wf); err == nil && ref.Hash != "" {
		r := ref
		dv.ChangeSet = &r
	}
	return dv, nil
}

// prepareFinish validates the FinishDiscussionCommand input and drives the
// managed structured resume on the SAME Provider Session that produces the
// ArtifactDiscussionHandoff (design §9.4, TUI task 12). A caller-supplied
// hand-written body is never the authority: the Application freezes the
// Change Set (already frozen before Finish), re-verifies the Workspace
// before and after the managed resume, binds the authoritative runtime
// facts (workflow, session, frozen Change Set), and validates the managed
// output against discussion-handoff.json.
func (a *Application) prepareFinish(ctx context.Context, c FinishDiscussionCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if !c.Session.Valid() {
		return nil, "", model.InvalidInputFault("finishing a discussion requires the bound session identity")
	}
	if len(c.Decisions) == 0 || len(c.Decisions) > 64*1024 {
		return nil, "", model.InvalidInputFault("the discussion decisions are required and bounded")
	}
	if !json.Valid(c.Decisions) {
		return nil, "", model.NewFault(model.CodeSchemaInvalid, "the discussion decisions are not canonical JSON")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf {
		return nil, "", model.InvalidInputFault("the workflow does not exist")
	}
	session := findSessionState(view.State, c.Session)
	if session == nil {
		return nil, "", model.InvalidInputFault("the discussion session is not bound to this workflow")
	}
	if session.Status != model.SessionStarting && session.Status != model.SessionInteractiveIdle {
		return nil, "", model.InvalidInputFault("finishing requires a resumable discussion session; the session is " + string(session.Status))
	}
	if session.ProviderSessionID == "" {
		return nil, "", model.NewFault(model.CodeProviderSessionIDMissing,
			"the discussion session has no bound provider session")
	}
	// The frozen Change Set is required: the managed handoff references it.
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, "", err
	}
	csRef, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactChangeSet})
	if err != nil {
		return nil, "", model.NewFault(model.CodeInvalidInput, "no frozen change set exists; freeze the change set before finishing")
	}
	// Re-verify the Workspace before the managed resume.
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return nil, "", err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return nil, "", err
	}
	// Drive the managed structured resume on the SAME Provider session.
	rt, err := a.agentRuntime(ctx, view.State)
	if err != nil {
		return nil, "", err
	}
	if rt == nil {
		return nil, "", model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	defer rt.Close()
	prompt, ok := a.planningPrompt(model.PrepareNativeDiscussionInput{})
	if !ok {
		return nil, "", model.InvalidInputFault("no embedded prompt for the discussion finish")
	}
	finishInput := struct {
		ChangeSet map[string]any `json:"change_set"`
		Decisions json.RawMessage `json:"decisions"`
	}{
		ChangeSet: map[string]any{"revision": csRef.Revision, "sha256": csRef.Hash},
		Decisions: c.Decisions,
	}
	res, err := rt.Resume(ctx, agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID(session.ProviderSessionID),
		Purpose:           model.PurposePlanning,
		Provider:          session.Provider,
		Prompt:            renderPrompt(prompt.Body, finishInput),
		Input:             finishInput,
		CWD:               cwd,
	})
	if err != nil {
		return nil, "", err
	}
	// Re-verify the Workspace after the managed resume: an unexpected change
	// rejects the Finish output (design 9.4: the managed resume must not
	// mutate the Workspace).
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return nil, "", err
	}
	handoff, err := assembleManagedHandoff(res, wf, c.Session, csRef)
	if err != nil {
		return nil, "", err
	}
	// The strict schema is the gate on the MANAGED output.
	if err := artifact.ValidateBody("discussion-handoff.json", handoff); err != nil {
		return nil, "", err
	}
	return model.FinishDiscussionInput{Session: c.Session, Handoff: handoff}, wf, nil
}

// assembleManagedHandoff extracts the managed structured resume output and
// assembles the canonical handoff body, binding the authoritative runtime
// facts (workflow, session, frozen Change Set). The resume output may
// carry the content fields directly or under a "discussion_handoff" key;
// it must never bind the runtime-fact keys.
func assembleManagedHandoff(res *agent.ResumeResult, wf model.WorkflowID, session model.SessionID, csRef model.ArtifactRef) ([]byte, error) {
	if res == nil || res.Run == nil || res.Run.Terminal == nil {
		return nil, model.NewFault(model.CodeSchemaInvalid,
			"the managed discussion resume produced no output")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Run.Terminal.Result), &out); err != nil || out == nil {
		return nil, model.NewFault(model.CodeSchemaInvalid,
			"the managed discussion resume output is not a JSON object")
	}
	if inner, ok := out["discussion_handoff"]; ok {
		if m, ok := inner.(map[string]any); ok {
			out = m
		}
	}
	for _, k := range []string{"workflow_id", "session_id", "change_set"} {
		if _, ok := out[k]; ok {
			return nil, model.NewFault(model.CodeSchemaInvalid,
				"the managed handoff must not bind the runtime fact "+k)
		}
	}
	body, err := json.Marshal(map[string]any{
		"workflow_id":         string(wf),
		"session_id":          string(session),
		"targets":             out["targets"],
		"constraints":         out["constraints"],
		"non_goals":           out["non_goals"],
		"acceptance_criteria": out["acceptance_criteria"],
		"open_questions":      out["open_questions"],
		"change_set":          map[string]any{"revision": csRef.Revision, "sha256": csRef.Hash},
		"user_decisions":      out["user_decisions"],
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// resolvePlanGenerationInputs resolves the immutable discussion inputs
// Plan generation is gated on (design §9.4, TUI task 12): the
// ArtifactDiscussionHandoff and the frozen ArtifactChangeSet. A workflow
// with an in-progress native discussion (a resumable planning Session with
// a bound Provider Session) requires BOTH before a Plan may be generated;
// a finished native discussion binds both Revisions. A legacy headless
// workflow (no handoff) keeps the legacy plan input — the Change Set alone
// is not a plan-generation gate there.
func (a *Application) resolvePlanGenerationInputs(ctx context.Context, wf model.WorkflowID) (handoff, cs model.ArtifactRef, err error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	handoffRef, herr := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactDiscussionHandoff})
	csRef, cerr := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactChangeSet})
	hasHandoff := herr == nil && handoffRef.Hash != ""
	hasChangeSet := cerr == nil && csRef.Hash != ""
	native, err := a.hasNativeDiscussion(ctx, wf)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	if native {
		if !hasHandoff || !hasChangeSet {
			return model.ArtifactRef{}, model.ArtifactRef{}, model.NewFault(model.CodeApprovalInputChanged,
				"plan generation requires both the discussion handoff and the frozen change set")
		}
		return handoffRef, csRef, nil
	}
	if hasHandoff {
		if !hasChangeSet {
			return model.ArtifactRef{}, model.ArtifactRef{}, model.NewFault(model.CodeApprovalInputChanged,
				"plan generation requires both the discussion handoff and the frozen change set")
		}
		return handoffRef, csRef, nil
	}
	// Legacy headless discussion: no handoff exists; the plan input falls
	// back to the discussion turn (backward compatibility for the headless
	// CLI, AGENTS.md: the headless CLI remains the official entry for
	// scripts, diagnostics, and automation).
	return model.ArtifactRef{}, model.ArtifactRef{}, nil
}

// hasNativeDiscussion reports whether the workflow carries an in-progress
// native discussion: a planning Session that is still resumable
// (non-terminal) with a bound Provider Session identity. The aggregate read
// failure is propagated — an unreadable aggregate must fail closed, never
// silently classify the workflow as legacy headless.
func (a *Application) hasNativeDiscussion(ctx context.Context, wf model.WorkflowID) (bool, error) {
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return false, err
	}
	for i := range view.State.Sessions {
		s := view.State.Sessions[i]
		if s.Purpose == model.PurposePlanning && s.ProviderSessionID != "" && !s.Status.IsTerminal() {
			return true, nil
		}
	}
	return false, nil
}

// findSessionState returns one Session of the aggregate.
func findSessionState(state model.State, id model.SessionID) *model.Session {
	for i := range state.Sessions {
		if state.Sessions[i].ID == id {
			return &state.Sessions[i]
		}
	}
	return nil
}

// nativeBridgeRequest assembles the managed Native Session Bridge
// request facts of one prepared native discussion Session (design §9.1,
// TUI task 12): the recorded Session binding (Provider and the
// bootstrap Provider Session identity), the Workflow Workspace the
// interactive process runs in, and the Adapter/Supervisor seams. The
// Bridge request is Application-owned state; the TUI only attaches the
// terminal streams and the user intent inside its blocking-exec
// callback. st is the already-open write Store of the command's mutation
// (no second Schema lock may be taken while the mutation locks are held).
func (a *Application) nativeBridgeRequest(ctx context.Context, st *store.Store, wf model.WorkflowID, session model.SessionID) *NativeBridgeRequest {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return nil
	}
	s := findSessionState(view.State, session)
	if s == nil || s.Provider == "" || s.ProviderSessionID == "" {
		return nil
	}
	ad := a.agent.Adapters[s.Provider]
	if ad == nil {
		return nil
	}
	return &NativeBridgeRequest{
		Workflow:        wf,
		Session:         s.ID,
		Provider:        s.Provider,
		ProviderSession: agent.ProviderSessionID(s.ProviderSessionID),
		Worktree:        a.layout.Workspace(wf),
		Adapter:         ad,
		Supervisor:      a.supervisor,
	}
}
