// Package app implements the Application seam (design 5): the concrete
// command surface the CLI calls. Query and Command are closed domain
// unions; there is no stringly typed registry and no extension hook.
//
// Execute routes every mutation through the same protocol (design 6.2):
// Recovery-before-mutation hook, the command lock matrix (design 6.1),
// current fact snapshot, Store.Transact(Decide), typed Effect dispatch,
// evidence validation, and Store.Transact(Decide EffectResult) until no
// Effect remains. Read Commands never migrate and never take writer
// locks; when no safe Reader exists only help and doctor remain.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// Application is the concrete command seam (design 5). All dependencies
// are private; callers only Query and Execute. One Application serves one
// Project of one CFLOW_HOME.
type Application struct {
	home       string
	project    Project
	dbPath     string
	cflowVer   string
	now        func() time.Time
	ids        model.IDSource
	redaction  security.Registry
	recoverer  Recoverer
	supervisor process.Supervisor
	probe      probe // test seam: protocol-order observation, nil in production

	mu     sync.Mutex
	locks  *platform.LockSet
	stores map[model.WorkflowID]*store.Store // open write Stores, per workflow
	known  map[model.WorkflowID]struct{}     // workflows opened this session
	procs  map[model.ProcessID]process.Handle
}

// probe is the unexported protocol-order observation seam (design 22.1:
// tests assert through observable facts). It is nil in production.
type probe interface {
	step(name string)
	lockKind(kind platform.LockKind)
}

func (a *Application) probeStep(name string) {
	if a.probe != nil {
		a.probe.step(name)
	}
}

func (a *Application) probeLockKind(k platform.LockKind) {
	if a.probe != nil {
		a.probe.lockKind(k)
	}
}

// New constructs the Application for one Project. A nil Recoverer uses
// the Task 7 recovery hook (home posture and schema compatibility); the
// full Recovery Engine arrives with Task 13.
func New(opts Options) (*Application, error) {
	if opts.Home == "" {
		return nil, model.InvalidInputFault("application requires a home directory")
	}
	if opts.Project.Key == "" || opts.Project.Root == "" {
		return nil, model.InvalidInputFault("application requires a project identity")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ids := opts.IDs
	if ids == nil {
		ids = model.SequentialIDSource()
	}
	sup := opts.Supervisor
	if sup == nil {
		sup = process.NewSupervisor(process.NewOSAdapter())
	}
	ver := opts.CflowVersion
	if ver == "" {
		ver = "0.0.0-dev"
	}
	a := &Application{
		home:       opts.Home,
		project:    opts.Project,
		dbPath:     filepath.Join(opts.Home, "cflow.db"),
		cflowVer:   ver,
		now:        now,
		ids:        ids,
		redaction:  opts.Redaction,
		supervisor: sup,
		stores:     map[model.WorkflowID]*store.Store{},
		known:      map[model.WorkflowID]struct{}{},
		procs:      map[model.ProcessID]process.Handle{},
	}
	if opts.Recoverer != nil {
		a.recoverer = opts.Recoverer
	} else {
		a.recoverer = &defaultRecoverer{home: opts.Home, dbPath: a.dbPath}
	}
	return a, nil
}

// lockSet opens the process LockSet lazily: reads on a database that does
// not exist never create CFLOW_HOME.
func (a *Application) lockSet() (*platform.LockSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.locks != nil {
		return a.locks, nil
	}
	s, err := platform.OpenLockSet(filepath.Join(a.home, "locks"), nil)
	if err != nil {
		return nil, err
	}
	a.locks = s
	return s, nil
}

// ---------------------------------------------------------------------------
// Query: closed read projections (design 6.1, 20)
// ---------------------------------------------------------------------------

// Query runs one closed read projection. Project reads take the shared DB
// Schema Lock, never migrate, and never take writer locks; when the
// database schema cannot be interpreted safely the read fails closed with
// a typed Fault.
func (a *Application) Query(ctx context.Context, q Query) (View, error) {
	switch qq := q.(type) {
	case ListQuery:
		return a.queryList(ctx, qq)
	case StatusQuery:
		return a.queryStatus(ctx, qq)
	case InspectQuery:
		return a.queryInspect(ctx, qq)
	case LogsQuery:
		return a.queryLogs(ctx, qq)
	default:
		return nil, model.InvalidInputFault("unsupported query")
	}
}

func (a *Application) queryList(ctx context.Context, q ListQuery) (View, error) {
	var out ListView
	for _, wf := range a.knownWorkflows() {
		view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
		if err != nil {
			return nil, orCtx(ctx, err)
		}
		if view.State.Workflow.ID == "" {
			continue // an orphaned workflow directory: Recovery's concern
		}
		out.Workflows = append(out.Workflows, workflowSummary(view.State))
	}
	return out, nil
}

func (a *Application) queryStatus(ctx context.Context, q StatusQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return StatusView{}, nil // project-level empty projection
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	return statusView(view.State), nil
}

func (a *Application) queryInspect(ctx context.Context, q InspectQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return InspectView{}, nil
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	pending := make([]string, 0, len(view.PendingEffects))
	for _, pe := range view.PendingEffects {
		pending = append(pending, fmt.Sprintf("%T", pe.Intent))
	}
	return inspectView(view.State, pending), nil
}

func (a *Application) queryLogs(ctx context.Context, q LogsQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return LogsView{}, nil
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{From: q.From, Limit: q.Limit})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	return LogsView{Events: view.Events, NextEventSeq: view.NextEventSeq}, nil
}

// readAggregate hydrates one workflow through a read-only Store open
// (which never migrates, design 6.1) under the shared DB Schema Lock. A
// missing database is an empty projection and creates nothing.
func (a *Application) readAggregate(ctx context.Context, wf model.WorkflowID, sq store.StoreQuery) (store.StoreView, error) {
	if _, err := os.Stat(a.dbPath); err != nil {
		return store.StoreView{}, nil
	}
	ls, err := a.lockSet()
	if err != nil {
		return store.StoreView{}, err
	}
	hold, err := ls.SchemaShared(ctx)
	if err != nil {
		return store.StoreView{}, lockFault(err)
	}
	defer hold.Release()
	a.probeStep("lock")
	a.probeLockKind(platform.LockSchema)
	st, err := store.Open(ctx, store.OpenOptions{
		Path: a.dbPath, Workflow: wf, ReadOnly: true, CflowVersion: a.cflowVer,
	})
	if err != nil {
		return store.StoreView{}, err
	}
	defer st.Close()
	return st.View(ctx, sq)
}

// ---------------------------------------------------------------------------
// Execute: recovery hook, lock matrix, transaction/effect loop (design 6)
// ---------------------------------------------------------------------------

// Execute runs one closed mutation command. Every mutation follows the
// same protocol: Recovery-before-mutation hook, command locks, current
// fact snapshot, Store.Transact(Decide), typed Effect dispatch, evidence
// validation, Store.Transact(Decide EffectResult) until no Effect
// remains.
func (a *Application) Execute(ctx context.Context, cmd Command) (Outcome, error) {
	input, wf, err := a.prepare(cmd)
	if err != nil {
		return Outcome{}, orCtx(ctx, err)
	}
	restricted := false
	if err := a.recoverReconcile(ctx); err != nil {
		code, _ := model.CodeOf(err)
		if safetyPathAllowed(cmd, code) {
			restricted = true
		} else {
			return Outcome{}, orCtx(ctx, err)
		}
	}
	st, err := a.ensureWriteStore(ctx, wf)
	if err != nil {
		return Outcome{}, orCtx(ctx, err)
	}
	holds, err := a.acquireMutationLocks(ctx, wf)
	if err != nil {
		return Outcome{}, orCtx(ctx, err)
	}
	defer releaseHolds(holds)
	out, err := a.runDecisionLoop(ctx, st, wf, input, restricted)
	if err != nil {
		return Outcome{}, orCtx(ctx, err)
	}
	// The events.jsonl export is generated from the committed Event
	// sequence and is never read by Recovery (design 21); a failure is a
	// warning, never a mutation failure.
	if len(out.Events) > 0 {
		out.ExportErr = a.exportEvents(ctx, wf, out.Events)
	}
	return out, orCtx(ctx, nil)
}

// recoverReconcile runs the Recovery-before-mutation hook while holding
// the shared DB Schema Lock (design 9.3 step 1: version detection under
// the shared lock).
func (a *Application) recoverReconcile(ctx context.Context) error {
	a.probeStep("recover")
	ls, err := a.lockSet()
	if err != nil {
		return err
	}
	hold, err := ls.SchemaShared(ctx)
	if err != nil {
		return lockFault(err)
	}
	defer hold.Release()
	return a.recoverer.Reconcile(ctx)
}

// ensureWriteStore opens (and migrates) the Store for one workflow.
// Migration is automatic before a stateful write under the exclusive DB
// Schema Lock only (design 6.1, 9.3).
func (a *Application) ensureWriteStore(ctx context.Context, wf model.WorkflowID) (*store.Store, error) {
	if wf == "" {
		return nil, model.InvalidInputFault("mutation requires a workflow identity")
	}
	a.mu.Lock()
	if s := a.stores[wf]; s != nil {
		a.mu.Unlock()
		return s, nil
	}
	a.mu.Unlock()
	a.probeStep("migration")
	ls, err := a.lockSet()
	if err != nil {
		return nil, err
	}
	hold, err := ls.SchemaExclusive(ctx)
	if err != nil {
		return nil, lockFault(err)
	}
	defer hold.Release()
	st, err := store.Open(ctx, store.OpenOptions{
		Path: a.dbPath, Workflow: wf, CflowVersion: a.cflowVer, Now: a.now,
	})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.stores[wf] = st
	a.known[wf] = struct{}{}
	a.mu.Unlock()
	return st, nil
}

// acquireMutationLocks takes a mutation's lock batch in the fixed order
// (design 18.1): shared Schema, Project Writer, Workflow Owner. Apply
// mutations add the Integration/Apply Lock in a later task.
func (a *Application) acquireMutationLocks(ctx context.Context, wf model.WorkflowID) ([]*platform.Hold, error) {
	ls, err := a.lockSet()
	if err != nil {
		return nil, err
	}
	var holds []*platform.Hold
	for _, take := range []func(context.Context) (*platform.Hold, error){
		ls.SchemaShared,
		func(ctx context.Context) (*platform.Hold, error) { return ls.ProjectWriter(ctx, a.project.Key) },
		func(ctx context.Context) (*platform.Hold, error) {
			return ls.WorkflowOwner(ctx, a.project.Key, string(wf))
		},
	} {
		h, err := take(ctx)
		if err != nil {
			releaseHolds(holds)
			return nil, lockFault(err)
		}
		holds = append(holds, h)
	}
	a.probeStep("lock")
	for _, k := range []platform.LockKind{platform.LockSchema, platform.LockProjectWriter, platform.LockWorkflowOwner} {
		a.probeLockKind(k)
	}
	return holds, nil
}

// runDecisionLoop applies Decisions until the Kernel requests no Effect,
// executing each committed Intent exactly once and feeding its immutable
// Result back as evidence for the next Decision (design 6.2). The loop is
// bounded by the persisted Intent ledger and rejects repeated identical
// uncompleted Intent identity.
func (a *Application) runDecisionLoop(ctx context.Context, st *store.Store, wf model.WorkflowID, input model.Input, restricted bool) (Outcome, error) {
	// Current fact snapshot: the aggregate version and the persisted
	// pending Intent ledger bound the loop.
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	version := view.AggregateVersion
	budget := effectBudget(view.State, len(view.PendingEffects))
	executed := map[string]struct{}{}
	var committed []model.CommittedDecision
	resultFed := false
	for iter := 0; iter < budget; iter++ {
		cd, err := st.Transact(ctx, version, func(state model.State) (model.Decision, error) {
			d, err := decision.Decide(state, input)
			if err != nil {
				return model.Decision{}, err
			}
			return a.completeCreateIdentity(input, d), nil
		})
		if err != nil {
			return Outcome{}, err
		}
		committed = append(committed, cd)
		version = cd.Version
		if resultFed {
			a.probeStep("result-commit")
			resultFed = false
		}
		if cd.Decision.Effect == nil {
			break
		}
		id := intentIdentity(cd.Decision.Effect)
		if _, dup := executed[id]; dup {
			return Outcome{}, model.InvariantFault(fmt.Errorf("repeated identical uncompleted effect intent %s", id))
		}
		executed[id] = struct{}{}
		a.probeStep("intent-commit")
		a.probeStep("effect")
		result, err := a.executeEffect(ctx, cd.Decision.Effect, restricted)
		if err != nil {
			return Outcome{}, err
		}
		if err := validateEffectResult(cd.Decision.Effect, result); err != nil {
			return Outcome{}, err
		}
		input = model.EffectResultInput(result)
		resultFed = true
	}
	if resultFed {
		// The budget ran out with a committed Intent still awaiting its
		// Result Decision: the Kernel requested more Effects than the
		// aggregate can justify.
		return Outcome{}, model.InvariantFault(fmt.Errorf("effect loop exceeded the persisted-intent bound"))
	}

	final, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{
		Workflow:   wf,
		Stage:      final.State.Workflow.Stage,
		Runtime:    final.State.Workflow.Runtime,
		Restricted: restricted,
		Findings:   final.State.Findings,
	}
	for _, cd := range committed {
		out.Events = append(out.Events, cd.Decision.Events...)
	}
	if n := len(final.State.CleanupAttempts); n > 0 {
		last := final.State.CleanupAttempts[n-1]
		out.Cleanup = &last
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// command classification
// ---------------------------------------------------------------------------

// prepare classifies one Command into its kernel Input and its workflow
// identity. Identity that the Runtime owns (the opaque Workflow ID) is
// fixed before any Effect (design 6.2 rule 6).
func (a *Application) prepare(cmd Command) (model.Input, model.WorkflowID, error) {
	switch c := cmd.(type) {
	case CreateWorkflowCommand:
		if c.TargetBranch == "" || c.BaseCommit == "" {
			return nil, "", model.InvalidInputFault("workflow creation requires a target branch and base commit")
		}
		wf := model.WorkflowID(a.ids(model.IDWorkflow))
		return model.WorkflowCommandInput{
			Kind: model.CreateWorkflow, Workflow: wf,
			Project:      model.ProjectID(a.project.Key),
			TargetBranch: c.TargetBranch, BaseCommit: c.BaseCommit,
		}, wf, nil
	case StartWorkflowCommand:
		return a.workflowCommand(model.StartWorkflow, c.Workflow, "")
	case PauseWorkflowCommand:
		return a.workflowCommand(model.PauseWorkflow, c.Workflow, "")
	case ResumeWorkflowCommand:
		return a.workflowCommand(model.ResumeWorkflow, c.Workflow, "")
	case CancelWorkflowCommand:
		return a.workflowCommand(model.CancelWorkflow, c.Workflow, c.Reason)
	case DryRunCommand:
		wf, err := a.resolveMutationWorkflow(c.Workflow)
		if err != nil {
			return nil, "", err
		}
		return model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: c.Items}, wf, nil
	default:
		return nil, "", model.InvalidInputFault("unsupported command")
	}
}

func (a *Application) workflowCommand(kind model.WorkflowCommandKind, wf model.WorkflowID, reason string) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(wf)
	if err != nil {
		return nil, "", err
	}
	return model.WorkflowCommandInput{
		Kind: kind, Workflow: resolved,
		Project: model.ProjectID(a.project.Key), Reason: reason,
	}, resolved, nil
}

// completeCreateIdentity fills the Workflow identity a create Decision
// cannot derive from the empty aggregate (design 7.2: opaque IDs arrive
// through Inputs; design 6.2 rule 6: fixed before any Effect). The Kernel
// validates the input identity; the Store requires the create mutation
// and its Events to carry it.
func (a *Application) completeCreateIdentity(input model.Input, d model.Decision) model.Decision {
	wc, ok := input.(model.WorkflowCommandInput)
	if !ok || wc.Kind != model.CreateWorkflow || wc.Workflow == "" {
		return d
	}
	for i, m := range d.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok {
			wm.ID = wc.Workflow
			wm.Project = wc.Project
			wm.TargetBranch = wc.TargetBranch
			wm.BaseCommit = wc.BaseCommit
			d.Mutations[i] = wm
		}
	}
	for i := range d.Events {
		d.Events[i].Workflow = wc.Workflow
	}
	return d
}
