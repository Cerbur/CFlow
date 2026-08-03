// The Final Execution Report read model (Task 18, design 21, PRD 最终报
// 告示例): the immutable report is a pure read over approved Artifacts,
// database state, Git facts, Sessions, Attempts, Findings, verification
// manifests, migration compatibility, security posture, permissions, and
// Apply state. GenerateReport never changes Workflow state — it is a
// deterministic function of its input — and RenderMarkdown passes every
// free-form value through the Redactor before display (design 19.2). The
// Apply outcome renders as not run until the Gate 3 protected Apply
// lands.

package observe

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"cflow.local/cflow/internal/model"
)

// ReportInput is the typed read-model input the Application assembles:
// the aggregate state, the active approved Artifact references, the
// persisted Verification manifests, the migration and security postures,
// the Provider default-permission trust boundary, and the Event export
// facts. Nothing in it is produced by report generation itself.
type ReportInput struct {
	Build       BuildInfo
	GeneratedAt time.Time
	State       model.State

	// ActiveArtifacts are the active immutable Artifact references of the
	// workflow (plan, spec, catalog, workflow, routing, budget, report).
	ActiveArtifacts []model.ArtifactRef
	// VerificationManifests are the persisted Evidence Manifests of the
	// verification Nodes (design 16.2).
	VerificationManifests []model.EvidenceManifest

	Migration ReportMigration
	Security  ReportSecurity

	// TrustBoundary is the Provider default-permission disclosure (PRD
	// 约束 30). An empty value falls back to the canonical statement.
	TrustBoundary string

	EventExport ReportEventExport
}

// ReportMigration is the State Compatibility posture: the applied schema
// version, whether the embedded migration registry checksums were
// verified, the applied migration rows, and whether the latest migration
// backup was verified (design 9, PRD 最终报告示例).
type ReportMigration struct {
	SchemaVersion     int
	ChecksumsVerified bool
	Applied           []AppliedMigration
	BackupVerified    bool
}

// AppliedMigration is one applied schema_migrations row.
type AppliedMigration struct {
	Version int
	ID      string
	SHA256  string
}

// ReportSecurity is the Local Data Protection posture (design 19.1):
// managed directory/file modes, the Redactor rule revision, whether any
// raw Provider frame was persisted, and the at-rest encryption fact.
type ReportSecurity struct {
	HomeMode           string
	FileMode           string
	RedactionRevision  string
	RawFramesPersisted bool
	AtRestEncryption   string
}

// ReportEventExport names the rebuildable events.jsonl export and its
// authoritative Event sequence range (design 21).
type ReportEventExport struct {
	Path   string
	From   uint64
	To     uint64
	Stable bool
}

// Report is the immutable final execution report read model. Result is
// the derived outcome classification: PASSED for a COMPLETED Workflow,
// FAILED when a Blocking Finding or a failed Node exists, RUNNING while
// the Workflow is active, PENDING otherwise.
type Report struct {
	SchemaVersion string
	GeneratedAt   time.Time
	Build         BuildInfo

	Result   string
	Workflow ReportWorkflow
	Summary  ReportSummary

	Approvals    []ReportApproval
	Artifacts    []ReportArtifact
	Commits      []ReportCommit
	CommitPolicy ReportCommitPolicy
	Verification []ReportVerification
	Sessions     []ReportSession
	Findings     []ReportFinding

	Migration   ReportMigration
	Security    ReportSecurity
	Permissions ReportPermissions
	Apply       ReportApply
	Cleanup     ReportCleanup
	Risks       []string
	EventExport ReportEventExport
}

// ReportWorkflow is the immutable lifecycle facts of the report.
type ReportWorkflow struct {
	ID                model.WorkflowID
	Stage             model.WorkflowStage
	Runtime           model.RuntimeStatus
	TargetBranch      string
	BaseCommit        string
	IntegrationBranch string
	IntegrationHead   string
	PlanRevision      int
}

// ReportSummary is the aggregate count summary (PRD 最终报告示例).
type ReportSummary struct {
	Tasks     int
	Completed int
	Retries   int
	Sessions  int
	Duration  string
}

// ReportApproval is one append-only user Approval.
type ReportApproval struct {
	ID   model.ApprovalID
	Kind string
	Seq  uint64
}

// ReportArtifact is one approved Artifact row. Stale reports that the
// active Revision's hash no longer matches the Execution Approval's
// bound hash — the reference can never be silently treated as approved.
type ReportArtifact struct {
	Type     model.ArtifactType
	Revision int
	Hash     string
	Approved bool
	Stale    bool
}

// ReportCommit is one Task's delivery commits: the implementation Commit
// heads of its succeeded Attempts and the serial Merge Commit.
type ReportCommit struct {
	Task      string
	Commits   []string
	MergeHead string
}

// ReportCommitPolicy is the Commit Policy posture: the bound Preflight
// Revision and fingerprint, the verified Commit count, policy
// mismatches, and quarantined Branches.
type ReportCommitPolicy struct {
	PreflightRevision   int
	Fingerprint         string
	VerifiedCommits     int
	PolicyMismatches    int
	QuarantinedBranches int
}

// ReportVerification is one deterministic Verification row plus its
// independent semantic review outcome.
type ReportVerification struct {
	Node      string
	CommandID string
	Purpose   string
	Passed    bool
	Hash      string
	Review    string // "passed", "failed", or "none"
}

// ReportSession is one Agent Session of the workflow.
type ReportSession struct {
	ID       model.SessionID
	Purpose  model.AgentPurpose
	Provider string
	Status   model.SessionStatus
}

// ReportFinding is one persisted Finding.
type ReportFinding struct {
	ID       model.FindingID
	Code     model.Code
	Scope    model.FaultScope
	Subject  string
	Blocking bool
	Text     string
	Seq      uint64
}

// ReportPermissions is the Provider default-permission trust boundary
// disclosure (PRD 约束 30, design 19.3).
type ReportPermissions struct {
	TrustBoundary string
}

// ReportApply is the Apply outcome: NOT_RUN until the Gate 3 protected
// Apply lands; the report never claims an apply that did not happen.
type ReportApply struct {
	Status string
	Detail string
}

// ReportCleanup is the Cleanup posture.
type ReportCleanup struct {
	Status string
	Detail string
}

// canonicalTrustBoundary is the default Provider default-permission
// disclosure every report must carry (PRD 约束 30: the final report must
// state this limit and never show unprovable conclusions).
const canonicalTrustBoundary = "agents run with the provider's default permissions and the user's existing provider configuration; CFlow provides no sandbox guarantee"

// GenerateReport assembles the immutable report read model from the
// typed input. It is a pure function: the input State is never mutated
// and report generation can never change Workflow state (design 21).
func GenerateReport(in ReportInput) (Report, error) {
	st := in.State
	report := Report{
		SchemaVersion: "1.0.0",
		GeneratedAt:   in.GeneratedAt,
		Build:         in.Build,
		Workflow: ReportWorkflow{
			ID: st.Workflow.ID, Stage: st.Workflow.Stage, Runtime: st.Workflow.Runtime,
			TargetBranch: st.Workflow.TargetBranch, BaseCommit: st.Workflow.BaseCommit,
			IntegrationBranch: st.Workflow.IntegrationBranch,
			IntegrationHead:   st.Workflow.IntegrationHead,
		},
		Migration:   in.Migration,
		Security:    in.Security,
		EventExport: in.EventExport,
		Apply:       ReportApply{Status: "NOT_RUN", Detail: "the protected apply (Gate 3) has not run"},
		Cleanup:     cleanupPosture(st),
	}
	if st.Plan != nil {
		report.Workflow.PlanRevision = st.Plan.Revision
	}
	report.Result = resultOf(st)
	report.Summary = summaryOf(st)
	report.Approvals = approvalsOf(st)
	report.Artifacts = artifactsOf(st, in.ActiveArtifacts)
	report.Commits = commitsOf(st)
	report.CommitPolicy = commitPolicyOf(st)
	report.Verification = verificationOf(st, in.VerificationManifests)
	report.Sessions = sessionsOf(st)
	report.Findings = findingsOf(st)
	boundary := in.TrustBoundary
	if boundary == "" {
		boundary = canonicalTrustBoundary
	}
	report.Permissions = ReportPermissions{TrustBoundary: boundary}
	report.Risks = risksOf(st)
	return report, nil
}

// resultOf classifies the Workflow outcome.
func resultOf(st model.State) string {
	switch st.Workflow.Stage {
	case model.StageCompleted:
		if st.Workflow.Runtime == model.RuntimeSucceeded {
			return "PASSED"
		}
		return "FAILED"
	}
	for _, f := range st.Findings {
		if f.Blocking {
			return "FAILED"
		}
	}
	for _, n := range st.Nodes {
		if n.Status == model.NodeFailed {
			return "FAILED"
		}
	}
	if st.Workflow.Runtime == model.RuntimeRunning || st.Workflow.Runtime == model.RuntimePaused {
		return "RUNNING"
	}
	return "PENDING"
}

// summaryOf counts the Tasks, completed Nodes, charged retries, Sessions,
// and the wall duration of the execution.
func summaryOf(st model.State) ReportSummary {
	s := ReportSummary{Tasks: 0, Completed: 0, Retries: 0, Sessions: len(st.Sessions)}
	for _, n := range st.Nodes {
		switch n.Kind {
		case model.NodeAgentTask:
			s.Tasks++
			if n.Status == model.NodeSucceeded {
				s.Completed++
			}
		}
	}
	for _, a := range st.Attempts {
		if a.RetryCharged {
			s.Retries++
		}
	}
	var first, last time.Time
	for _, r := range st.Runs {
		if first.IsZero() || r.StartedAt.Before(first) {
			first = r.StartedAt
		}
		if !r.EndedAt.IsZero() && (last.IsZero() || r.EndedAt.After(last)) {
			last = r.EndedAt
		}
	}
	if !first.IsZero() {
		end := last
		if end.IsZero() {
			end = st.Now
		}
		d := end.Sub(first)
		s.Duration = fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return s
}

func approvalsOf(st model.State) []ReportApproval {
	out := make([]ReportApproval, 0, len(st.Approvals))
	for _, ap := range st.Approvals {
		out = append(out, ReportApproval{ID: ap.ID, Kind: string(ap.Kind), Seq: ap.Seq})
	}
	return out
}

// artifactsOf builds one row per approved Artifact type: the active
// reference with its hash compared to the Execution Approval's bound
// hash (a mismatch is reported STALE, never silently treated as
// approved).
func artifactsOf(st model.State, active []model.ArtifactRef) []ReportArtifact {
	facts := st.Workflow.ExecutionFacts
	approved := func(typ model.ArtifactType) string {
		if facts == nil {
			return ""
		}
		switch typ {
		case model.ArtifactPlan:
			return facts.PlanHash
		case model.ArtifactSpec:
			if len(facts.SpecHashes) > 0 {
				return facts.SpecHashes[0]
			}
		case model.ArtifactCatalog:
			return facts.CatalogHash
		case model.ArtifactWorkflow:
			return facts.WorkflowHash
		case model.ArtifactRoutingPolicy:
			return facts.RoutingHash
		case model.ArtifactBudgetPolicy:
			return facts.BudgetHash
		}
		return ""
	}
	types := []model.ArtifactType{
		model.ArtifactPlan, model.ArtifactSpec, model.ArtifactCatalog,
		model.ArtifactWorkflow, model.ArtifactRoutingPolicy, model.ArtifactBudgetPolicy,
	}
	var out []ReportArtifact
	for _, typ := range types {
		want := approved(typ)
		var row ReportArtifact
		found := false
		for _, ref := range active {
			if ref.Type != typ {
				continue
			}
			found = true
			row = ReportArtifact{Type: ref.Type, Revision: ref.Revision, Hash: ref.Hash}
			if want != "" {
				row.Approved = ref.Hash == want
				row.Stale = ref.Hash != want
			}
			break
		}
		if !found {
			row = ReportArtifact{Type: typ, Revision: 0, Hash: "", Approved: want == "", Stale: want != ""}
		}
		out = append(out, row)
	}
	return out
}

// commitsOf lists each Task's implementation Commit heads (the succeeded
// Attempt end heads) and the serial Merge Commit that brought them in.
func commitsOf(st model.State) []ReportCommit {
	mergeHead := func(task model.NodeID) string {
		merge := model.NodeID("merge-" + strings.TrimPrefix(string(task), "task-"))
		for k, a := range st.Attempts {
			if k.Node == merge && a.Status == model.AttemptSucceeded && a.EndHead != "" {
				return a.EndHead
			}
		}
		return ""
	}
	ids := make([]model.NodeID, 0, len(st.Nodes))
	for id := range st.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []ReportCommit
	for _, id := range ids {
		n := st.Nodes[id]
		if n.Kind != model.NodeAgentTask {
			continue
		}
		var commits []string
		for k, a := range st.Attempts {
			if k.Node == id && a.Status == model.AttemptSucceeded && a.EndHead != "" {
				commits = append(commits, a.EndHead)
			}
		}
		sort.Strings(commits)
		out = append(out, ReportCommit{Task: string(id), Commits: commits, MergeHead: mergeHead(id)})
	}
	return out
}

// commitPolicyOf summarizes the Commit Policy posture from the bound
// facts and the attempt evidence.
func commitPolicyOf(st model.State) ReportCommitPolicy {
	p := ReportCommitPolicy{PreflightRevision: 0, VerifiedCommits: 0, QuarantinedBranches: len(st.Quarantines)}
	if facts := st.Workflow.ExecutionFacts; facts != nil {
		p.PreflightRevision = facts.PreflightRevision
		p.Fingerprint = facts.Fingerprint
	}
	for _, a := range st.Attempts {
		switch a.FailureCode {
		case model.CodeCommitPolicyMismatch:
			p.PolicyMismatches++
		}
		for _, e := range a.Evidence {
			if e.Kind == model.EvidenceCommit {
				p.VerifiedCommits++
			}
		}
	}
	return p
}

// verificationOf merges the persisted Evidence Manifests with the
// independent review outcome of each verification Node.
func verificationOf(st model.State, manifests []model.EvidenceManifest) []ReportVerification {
	out := make([]ReportVerification, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, ReportVerification{
			Node: string(m.Node), CommandID: m.CommandID, Purpose: m.Purpose,
			Passed: m.Passed, Hash: m.Hash,
			Review: reviewOutcomeOf(st, m.Node),
		})
	}
	return out
}

// reviewOutcomeOf derives the independent semantic review outcome of one
// verification Node from its review evidence.
func reviewOutcomeOf(st model.State, node model.NodeID) string {
	for k, a := range st.Attempts {
		if k.Node != node || a.Status != model.AttemptSucceeded {
			continue
		}
		for _, e := range a.Evidence {
			if e.Kind == model.EvidenceReviewResult {
				return "passed"
			}
		}
		if a.FailureCode != "" {
			return "failed"
		}
	}
	return "none"
}

func sessionsOf(st model.State) []ReportSession {
	out := make([]ReportSession, 0, len(st.Sessions))
	for _, s := range st.Sessions {
		out = append(out, ReportSession{
			ID: s.ID, Purpose: s.Purpose, Provider: s.Provider, Status: s.Status,
		})
	}
	return out
}

func findingsOf(st model.State) []ReportFinding {
	out := make([]ReportFinding, 0, len(st.Findings))
	for _, f := range st.Findings {
		out = append(out, ReportFinding{
			ID: f.ID, Code: f.Code, Scope: f.Scope, Subject: f.Subject,
			Blocking: f.Blocking, Text: f.Text, Seq: f.Seq,
		})
	}
	return out
}

// cleanupPosture derives the Cleanup status from the CleanupAttempts.
func cleanupPosture(st model.State) ReportCleanup {
	if len(st.CleanupAttempts) == 0 {
		return ReportCleanup{Status: "NOT_RUN", Detail: "no cleanup dry run produced"}
	}
	last := st.CleanupAttempts[len(st.CleanupAttempts)-1]
	return ReportCleanup{Status: "DRY_RUN_ONLY", Detail: "cleanup manifest " + last.Manifest.String() + " produced; execute lands with a later task"}
}

// risksOf assembles the Remaining Risks: every open Finding plus the
// non-apply posture.
func risksOf(st model.State) []string {
	var out []string
	for _, f := range st.Findings {
		kind := "non-blocking"
		if f.Blocking {
			kind = "blocking"
		}
		out = append(out, fmt.Sprintf("%s finding %s %s (%s)", kind, f.Code, f.Subject, f.Text))
	}
	for _, r := range st.Runs {
		if r.Status.IsTerminal() && r.Status != model.RunSucceeded {
			out = append(out, fmt.Sprintf("run %s ended %s", r.ID, r.Status))
		}
	}
	out = append(out, "the protected apply to the target branch has not run")
	return out
}
