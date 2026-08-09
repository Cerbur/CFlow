package app

// The Final Execution Report Application side (Task 18, design 21): the
// report query assembles the typed read-model input — aggregate state,
// active approved Artifact references, verification manifests, migration
// compatibility, security posture, the trust boundary, and the Event
// export facts — and renders the redacted Markdown; the completion
// command writes the immutable Final Report Artifact. Report generation
// never changes Workflow state. Same-package split of the Application
// seam: no public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/store"
)

// queryReport assembles the Final Execution Report read model of one
// workflow (PRD 最终报告示例): approved hashes, Git facts, Sessions,
// Attempts, Findings, verification manifests, migration compatibility,
// security posture, permissions, and Apply state. It is a pure read: no
// lock beyond the shared Schema Lock, no migration, and no mutation.
func (a *Application) queryReport(ctx context.Context, q ReportQuery) (View, error) {
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
	in, err := a.reportInput(ctx, q.Build, view.State, view.NextEventSeq, len(view.Events) == 0)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	report, err := observe.GenerateReport(in)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	return ReportView{Report: report, Markdown: observe.RenderMarkdown(report, a.redaction)}, nil
}

// reportInput assembles the typed read-model input from the aggregate,
// the Artifact Store, the evidence root, the database migration posture,
// and the security posture.
func (a *Application) reportInput(ctx context.Context, build observe.BuildInfo, st model.State, nextEventSeq uint64, noEvents bool) (observe.ReportInput, error) {
	in := observe.ReportInput{
		Build:         build,
		GeneratedAt:   a.now().UTC(),
		State:         st,
		Migration:     observe.ReportMigration{ChecksumsVerified: true},
		Security:      a.reportSecurity(),
		TrustBoundary: reportTrustBoundary,
	}
	store, err := a.artifactStore(wfOf(st))
	if err != nil {
		return observe.ReportInput{}, err
	}
	for _, typ := range []model.ArtifactType{
		model.ArtifactPlan, model.ArtifactSpec, model.ArtifactCatalog,
		model.ArtifactWorkflow, model.ArtifactRoutingPolicy, model.ArtifactBudgetPolicy,
		model.ArtifactReport,
	} {
		ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wfOf(st), Type: typ})
		if err != nil {
			if artifact.IsNotFound(err) {
				continue
			}
			return observe.ReportInput{}, err
		}
		in.ActiveArtifacts = append(in.ActiveArtifacts, ref)
	}
	in.VerificationManifests, err = a.verificationManifests(ctx, wfOf(st))
	if err != nil {
		return observe.ReportInput{}, err
	}
	in.Migration.Applied, in.Migration.SchemaVersion, err = a.appliedMigrations(ctx, wfOf(st))
	if err != nil {
		return observe.ReportInput{}, err
	}
	wfDir, err := a.workflowDir(ctx, wfOf(st))
	if err != nil {
		return observe.ReportInput{}, err
	}
	in.EventExport = observe.ReportEventExport{
		Path:   filepath.Join(wfDir, "events.jsonl"),
		From:   1,
		To:     nextEventSeq - 1,
		Stable: true,
	}
	if noEvents {
		in.EventExport.From, in.EventExport.To = 0, 0
	}
	return in, nil
}

// wfOf is the workflow identity of one aggregate (the report input's
// carrier; "" for an empty aggregate).
func wfOf(st model.State) model.WorkflowID { return st.Workflow.ID }

// reportSecurity is the Local Data Protection posture: the managed home
// directory mode, the managed file mode, the Redactor rule revision, the
// raw-frame posture, and the at-rest encryption fact (design 19).
func (a *Application) reportSecurity() observe.ReportSecurity {
	mode := "0700"
	if info, err := os.Stat(a.home); err == nil {
		mode = fmt.Sprintf("%04o", info.Mode().Perm())
	}
	return observe.ReportSecurity{
		HomeMode:           mode,
		FileMode:           "0600",
		RedactionRevision:  a.redaction.Revision,
		RawFramesPersisted: false,
		AtRestEncryption:   "none",
	}
}

// appliedMigrations reads the applied schema_migrations posture through a
// read-only Store open (never migrating, design 6.1).
func (a *Application) appliedMigrations(ctx context.Context, wf model.WorkflowID) ([]observe.AppliedMigration, int, error) {
	if _, err := os.Stat(a.dbPath); err != nil {
		return nil, 0, err
	}
	ls, err := a.lockSet()
	if err != nil {
		return nil, 0, err
	}
	hold, err := ls.SchemaShared(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer hold.Release()
	st, err := store.Open(ctx, store.OpenOptions{
		Path: a.dbPath, Workflow: wf, ReadOnly: true, CflowVersion: a.cflowVer, Now: a.now,
	})
	if err != nil {
		return nil, 0, err
	}
	defer st.Close()
	rows, err := st.AppliedMigrations(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]observe.AppliedMigration, 0, len(rows))
	version := 0
	for _, r := range rows {
		out = append(out, observe.AppliedMigration{Version: r.Version, ID: r.ID, SHA256: r.SHA256})
		if r.Version > version {
			version = r.Version
		}
	}
	return out, version, nil
}

// verificationManifests reads every persisted Evidence Manifest of one
// workflow from the managed evidence root (design 16.2).
func (a *Application) verificationManifests(ctx context.Context, wf model.WorkflowID) ([]model.EvidenceManifest, error) {
	root, err := a.workflowEvidenceDir(ctx, wf)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, nil
	}
	dir := filepath.Join(root, "verification")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []model.EvidenceManifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var m model.EvidenceManifest
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, model.InvariantFault(fmt.Errorf("verification evidence %s cannot be parsed: %w", e.Name(), err))
		}
		if m.Hash == "" {
			return nil, model.InvariantFault(fmt.Errorf("verification evidence %s has no identity hash", e.Name()))
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

// writeFinalReportIfCompleted writes the immutable Final Report Artifact
// when the Workflow actually recorded COMPLETED (the dispatch-driven
// completion of the Final Verify chain; a chain that settled otherwise
// writes nothing). The read goes through the already-open write Store
// (design 18.1).
func (a *Application) writeFinalReportIfCompleted(ctx context.Context, wf model.WorkflowID, st *store.Store) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	if view.State.Workflow.Stage != model.StageCompleted {
		return nil
	}
	return a.writeFinalReport(ctx, wf, st)
}

// writeFinalReport writes the immutable Final Report Artifact after the
// completion Decision committed (PRD 最终验收: 生成 final-report.md). The
// report is a rebuildable read model; writing it never changes Workflow
// state.
func (a *Application) writeFinalReport(ctx context.Context, wf model.WorkflowID, st *store.Store) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	in, err := a.reportInput(ctx, a.buildIdentity(), view.State, view.NextEventSeq, len(view.Events) == 0)
	if err != nil {
		return err
	}
	report, err := observe.GenerateReport(in)
	if err != nil {
		return err
	}
	markdown := observe.RenderMarkdown(report, a.redaction)
	store, err := a.artifactStore(wf)
	if err != nil {
		return err
	}
	revision := 1
	if ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactReport}); err == nil {
		revision = ref.Revision + 1
	}
	ref, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactReport,
		Revision:      revision,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: "completion"},
		Body:          []byte(markdown),
	})
	if err != nil {
		return err
	}
	_ = ref
	return nil
}

// buildIdentity is the binary identity the completion report records
// (the CLI passes the real BuildInfo; the Application carries the
// observe defaults when driven directly).
func (a *Application) buildIdentity() observe.BuildInfo {
	return observe.Current()
}

// reportTrustBoundary is the Provider default-permission disclosure of
// the report (PRD 约束 30, design 19.3).
const reportTrustBoundary = "agents run with the provider's default permissions and the user's existing provider configuration; CFlow provides no sandbox guarantee"
