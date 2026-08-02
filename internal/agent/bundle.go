// Immutable Context Bundle creation (design 14.4, PRD 已确认：Session Resume
// 失败与跨 Provider 上下文交接). A Context Bundle is a redacted, versioned
// handoff package: every update writes a new Revision file and never
// modifies a persisted Revision in place. The hash is the digest of the
// canonical manifest excluding its own field, so identical inputs with
// the same Clock produce byte-identical Revision manifests.
package agent

import (
	"context"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// CreateContextBundle builds, redacts, hashes, and persists one immutable
// Context Bundle Revision for a Session the Runtime knows. The Session's
// purpose must match the request; the caller (the Resume fallback) marks
// the Session LOST before building the bundle (design 14.4 step 1-2).
func (r *Runtime) CreateContextBundle(ctx context.Context, req ContextBundleRequest) (ContextBundle, error) {
	if err := ctx.Err(); err != nil {
		return ContextBundle{}, err
	}
	r.mu.Lock()
	rec := r.byCFlow[req.SessionID]
	r.mu.Unlock()
	if rec == nil {
		return ContextBundle{}, model.InvalidInputFault("unknown session for context bundle")
	}
	if rec.session.Purpose != req.Purpose {
		return ContextBundle{}, model.NewFault(model.CodeSessionIndependenceViolation,
			"a context bundle must keep the session's purpose")
	}
	if rec.session.ProviderSessionID != string(req.ProviderSessionID) {
		return ContextBundle{}, model.InvalidInputFault("provider session id does not match the session record")
	}

	red := security.NewRedactor(r.redaction)
	redacted, err := redactContextInput(red, req.Context)
	if err != nil {
		return ContextBundle{}, err
	}
	pb := persistedBundle{
		SchemaVersion:      bundleSchemaVersion,
		SessionID:          string(req.SessionID),
		ProviderSessionID:  string(req.ProviderSessionID),
		Purpose:            req.Purpose,
		CreatedAt:          r.now(),
		Requirement:        redacted.Requirement,
		Plan:               pinToPersisted(redacted.Plan),
		Spec:               pinToPersisted(redacted.Spec),
		Catalog:            pinToPersisted(redacted.Catalog),
		Workflow:           pinToPersisted(redacted.Workflow),
		RepositoryBaseline: redacted.RepositoryBaseline,
		StageSummary:       redacted.StageSummary,
		Decisions:          redacted.Decisions,
		FailureEvidence:    evidenceToPersisted(redacted.FailureEvidence),
		OpenQuestions:      redacted.OpenQuestions,
		PermissionBoundary: redacted.PermissionBoundary,
		RedactionRevision:  r.redaction.Revision,
	}
	revision, digest, path, err := r.evidence.writeBundle(req.SessionID, pb)
	if err != nil {
		return ContextBundle{}, err
	}
	return ContextBundle{
		SchemaVersion:     bundleSchemaVersion,
		Revision:          revision,
		Hash:              digest,
		Path:              path,
		SessionID:         req.SessionID,
		ProviderSessionID: req.ProviderSessionID,
		Purpose:           req.Purpose,
		CreatedAt:         pb.CreatedAt,
		Context:           redacted,
		RedactionRevision: r.redaction.Revision,
	}, nil
}

// redactContextInput redacts every free-text field of a bundle input
// through the Redactor (structured pins and evidence hashes are non-secret
// facts and pass through). Failures fail closed with the Redactor fault.
func redactContextInput(red *security.Redactor, in ContextInput) (ContextInput, error) {
	out := in
	var err error
	if out.Requirement, err = redactText(red, in.Requirement); err != nil {
		return ContextInput{}, err
	}
	if out.RepositoryBaseline, err = redactText(red, in.RepositoryBaseline); err != nil {
		return ContextInput{}, err
	}
	if out.StageSummary, err = redactText(red, in.StageSummary); err != nil {
		return ContextInput{}, err
	}
	if out.PermissionBoundary, err = redactText(red, in.PermissionBoundary); err != nil {
		return ContextInput{}, err
	}
	out.Decisions = make([]string, 0, len(in.Decisions))
	for _, d := range in.Decisions {
		rd, err := redactText(red, d)
		if err != nil {
			return ContextInput{}, err
		}
		out.Decisions = append(out.Decisions, rd)
	}
	out.OpenQuestions = make([]string, 0, len(in.OpenQuestions))
	for _, q := range in.OpenQuestions {
		rq, err := redactText(red, q)
		if err != nil {
			return ContextInput{}, err
		}
		out.OpenQuestions = append(out.OpenQuestions, rq)
	}
	return out, nil
}

// pinToPersisted converts an ArtifactPin; a zero pin is omitted.
func pinToPersisted(p ArtifactPin) *persistedPin {
	if p.Type == "" && p.Hash == "" {
		return nil
	}
	return &persistedPin{Type: p.Type, Revision: p.Revision, Hash: p.Hash}
}

// evidenceToPersisted converts failure evidence references.
func evidenceToPersisted(refs []model.EvidenceRef) []persistedEvidence {
	if len(refs) == 0 {
		return nil
	}
	out := make([]persistedEvidence, 0, len(refs))
	for _, ref := range refs {
		out = append(out, persistedEvidence{Kind: string(ref.Kind), Hash: ref.Hash, Subject: ref.Subject})
	}
	return out
}
