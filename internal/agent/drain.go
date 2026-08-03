package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// The unified pipeline drain (design 14.3): every Adapter stream is
// driven through sequence validation, redaction, and evidence persistence
// here. A validated event schema, not stdout prose or exit code, identifies
// the Session and structured completion.
//
// ---------------------------------------------------------------------------
// The unified pipeline drain (design 14.3)
// ---------------------------------------------------------------------------

// drainConfig fixes one stream's protocol facts.
type drainConfig struct {
	binding    ProviderBinding
	bound      ProviderSessionID // resume: the session to re-affirm
	session    *sessionRecord    // resume: the existing record
	purpose    model.AgentPurpose
	provider   string
	supersedes model.SessionID
	sessionID  model.SessionID // start: the Application-allocated CFlow Session identity
	promptHash string
	inputHash  string
	runID      model.RunID
	startedAt  time.Time
}

// drain drives one Adapter stream through the pipeline until the terminal
// event, a cancellation, or a fail-closed error: sequence validation,
// redaction, evidence persistence, and Session ledger settlement.
func (r *Runtime) drain(ctx context.Context, arun Run, cfg drainConfig) (*RunResult, error) {
	red := security.NewRedactor(r.redaction)
	seq := newProtocolSequence(cfg.binding, cfg.bound)
	lr := &liveRun{}
	r.mu.Lock()
	r.live[cfg.runID] = lr
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, cfg.runID)
		r.mu.Unlock()
	}()

	var (
		events   []Event
		terminal *Event
		session  *sessionRecord
		exitCode int
	)

	settle := func(status model.SessionStatus, exit int, findings []persistedFinding, bundle *persistedBundleRef) (*RunResult, error) {
		res, err := r.settleWith(settleArgs{
			cfg: cfg, session: session, status: status, exitCode: exit,
			findings: findings, bundle: bundle, events: events,
		})
		if err != nil {
			return nil, err
		}
		if res != nil {
			res.Terminal = terminal
			res.ExitCode = exit
		}
		return res, nil
	}

	for {
		if lr.isCancelled() {
			return settle(model.SessionCancelled, exitCode, nil, nil)
		}
		ev, err := arun.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) {
				return r.failProtocol(ctx, cfg, session, pe, events)
			}
			var crash *ProcessCrash
			if errors.As(err, &crash) {
				exitCode = crash.ExitCode
				findings := []persistedFinding{{Code: model.CodeAgentProcessCrashed, Text: crash.Message}}
				if _, serr := settle(model.SessionFailed, crash.ExitCode, findings, nil); serr != nil {
					return nil, serr
				}
				return nil, model.NewFault(model.CodeAgentProcessCrashed, crash.Message)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if err := seq.accept(&ev); err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) {
				return r.failProtocol(ctx, cfg, session, pe, events)
			}
			return nil, err
		}
		if ev.Type == EventSessionStarted {
			if cfg.session != nil {
				session = cfg.session
			} else {
				session, err = r.establish(ctx, cfg, ev.SessionID)
				if err != nil {
					return nil, err
				}
			}
			r.mu.Lock()
			session.runID = cfg.runID
			session.session.Status = model.SessionActive
			lr.setHandle(RunHandle{RunID: cfg.runID, Session: session.session.ID, ProviderSessionID: ev.SessionID})
			r.mu.Unlock()
		}
		redEv, err := redactEvent(red, &ev)
		if err != nil {
			if _, serr := settle(model.SessionFailed, exitCode, nil, nil); serr != nil {
				return nil, serr
			}
			return nil, err
		}
		events = append(events, *redEv)
		if err := r.persistEvent(ctx, cfg, session, redEv); err != nil {
			return nil, err
		}
		if redEv.Type == EventCompleted || redEv.Type == EventFailed {
			t := *redEv
			terminal = &t
		}
	}

	// Stream ended: the outcome is settled from the protocol facts.
	if lr.isCancelled() {
		return settle(model.SessionCancelled, exitCode, nil, nil)
	}
	if terminal == nil {
		// No terminal event: the process ended before structured
		// completion. The exit code cannot override this (PRD 约束 43).
		msg := "stream ended without a terminal event; exit code cannot complete the run"
		findings := []persistedFinding{{Code: model.CodeAgentProcessCrashed, Text: msg}}
		if _, serr := settle(model.SessionFailed, exitCode, findings, nil); serr != nil {
			return nil, serr
		}
		return nil, model.NewFault(model.CodeAgentProcessCrashed, msg)
	}
	status := model.SessionCompleted
	if terminal.Type == EventFailed {
		status = model.SessionFailed
	}
	return settle(status, exitCode, nil, nil)
}

// establish creates the CFlow Session the moment a validated
// session_started event claims its id (acceptance: Session ID appears
// through a validated start event). A claimed id already bound to another
// purpose violates Session independence; the same purpose must be
// resumed, never restarted.
func (r *Runtime) establish(ctx context.Context, cfg drainConfig, claimed ProviderSessionID) (*sessionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.byProvider[claimed]; ok {
		if rec.session.Purpose != cfg.purpose {
			return nil, model.NewFault(model.CodeSessionIndependenceViolation,
				"provider session id is already bound to another purpose")
		}
		return nil, model.InvalidInputFault("provider session id is already in use; resume it instead")
	}
	// The Application allocates the CFlow Session identity for every role
	// lineage (design 14.4); the Runtime honors it and only falls back to
	// its own source when the caller did not allocate one.
	id := cfg.sessionID
	if id == "" {
		id = model.SessionID(r.ids(model.IDSession))
	}
	rec := &sessionRecord{
		session: model.Session{
			ID:                id,
			ProviderSessionID: string(claimed),
			Purpose:           cfg.purpose,
			Status:            model.SessionActive,
			Supersedes:        cfg.supersedes,
		},
		provider: cfg.provider,
	}
	r.byProvider[claimed] = rec
	r.byCFlow[rec.session.ID] = rec
	return rec, nil
}

// failProtocol settles a protocol violation: the redacted raw evidence and
// the finding are persisted, the affected Session (when established) is
// FAILED, and the non-retryable protocol Fault is returned with its
// evidence (design 14.3, PRD 已确认 item 5).
func (r *Runtime) failProtocol(ctx context.Context, cfg drainConfig, session *sessionRecord, pe *ProtocolError, events []Event) (*RunResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctx.Err()
	}
	code := pe.Code
	if code != model.CodeProviderProtocolViolation && code != model.CodeProviderSessionIDMissing {
		code = model.CodeProviderProtocolViolation
	}
	subject := ""
	if session != nil {
		subject = session.session.ProviderSessionID
	} else if pe.SessionID != "" {
		subject = string(pe.SessionID)
	}
	frameHash := ""
	redacted := ""
	if len(pe.Frame) > 0 {
		frameHash = hashBytes(pe.Frame)
		if r.evidence != nil {
			if rf, err := redactText(security.NewRedactor(r.redaction), string(pe.Frame)); err == nil {
				redacted = rf
			}
		}
	}
	if r.evidence != nil {
		v := persistedViolation{
			At:            r.now(),
			SessionID:     ProviderSessionID(subject),
			Code:          code,
			FrameHash:     frameHash,
			RedactedFrame: redacted,
			Message:       pe.Message,
		}
		if err := r.evidence.appendViolation(v); err != nil {
			return nil, err
		}
	}
	findings := []persistedFinding{{Code: code, FrameHash: frameHash, Text: pe.Message}}
	if _, err := r.settleWith(settleArgs{cfg: cfg, session: session, status: model.SessionFailed, findings: findings, events: events}); err != nil {
		return nil, err
	}
	evidence := model.EvidenceRef{Kind: model.EvidenceProtocolEvent, Hash: frameHash, Subject: subject}
	return nil, model.NewFaultWithEvidence(code, pe.Message, evidence)
}

// settleWith is the manifest/close helper shared by the drain and the
// fail-closed paths.
type settleArgs struct {
	cfg      drainConfig
	session  *sessionRecord
	status   model.SessionStatus
	exitCode int
	findings []persistedFinding
	bundle   *persistedBundleRef
	events   []Event
}

// settleWith writes the session manifest and closes the events file for a
// final Session status. A stream that never established a Session (e.g. an
// empty stream) still yields the run result without a manifest.
func (r *Runtime) settleWith(a settleArgs) (*RunResult, error) {
	result := &RunResult{
		RunID:      a.cfg.runID,
		Provider:   a.cfg.provider,
		Status:     runStatusOf(a.status),
		Events:     a.events,
		ExitCode:   a.exitCode,
		PromptHash: a.cfg.promptHash,
		InputHash:  a.cfg.inputHash,
		StartedAt:  a.cfg.startedAt,
		EndedAt:    r.now(),
	}
	if a.session == nil || r.evidence == nil {
		return result, nil
	}
	r.mu.Lock()
	a.session.session.Status = a.status
	r.mu.Unlock()
	result.Session = a.session.session
	m := persistedSession{
		SessionID:         string(a.session.session.ID),
		Provider:          a.cfg.provider,
		ProviderSessionID: a.session.session.ProviderSessionID,
		Purpose:           a.session.session.Purpose,
		Status:            a.status,
		Supersedes:        string(a.session.session.Supersedes),
		StartedAt:         a.cfg.startedAt,
		EndedAt:           r.now(),
		ExitCode:          a.exitCode,
		PromptHash:        a.cfg.promptHash,
		InputHash:         a.cfg.inputHash,
		RedactionRevision: r.redaction.Revision,
		Findings:          a.findings,
		ContextBundle:     a.bundle,
	}
	for _, ev := range a.events {
		m.Events = append(m.Events, persistedEventRef{Seq: ev.Seq, Type: ev.Type, FrameHash: ev.FrameHash})
	}
	digest, err := r.evidence.writeManifest(a.session.session.ID, m)
	if err != nil {
		return nil, err
	}
	result.ManifestHash = digest
	if err := r.evidence.closeSession(a.session.session.ID); err != nil {
		return nil, err
	}
	return result, nil
}

// persistEvent appends one redacted complete event line plus its protocol
// hash and the redaction revision.
func (r *Runtime) persistEvent(ctx context.Context, cfg drainConfig, session *sessionRecord, ev *Event) error {
	if r.evidence == nil || session == nil {
		return nil
	}
	return r.evidence.appendEvent(ctx, session.session.ID, persistedEvent{
		Seq:               ev.Seq,
		Type:              ev.Type,
		SessionID:         ev.SessionID,
		AtMillis:          ev.AtMillis,
		Text:              ev.Text,
		Tool:              ev.Tool,
		Input:             ev.Input,
		Output:            ev.Output,
		InputTokens:       ev.InputTokens,
		OutputTokens:      ev.OutputTokens,
		CostUSD:           ev.CostUSD,
		Result:            ev.Result,
		Code:              ev.Code,
		Message:           ev.Message,
		FrameHash:         ev.FrameHash,
		RedactionRevision: r.redaction.Revision,
	})
}

// sessionStatusTerminal reports whether a Session status is final: a
// terminal Session can never be resumed, so it can never be re-activated
// and can never chain another successor lineage.
func sessionStatusTerminal(s model.SessionStatus) bool {
	switch s {
	case model.SessionCompleted, model.SessionFailed, model.SessionCancelled, model.SessionLost:
		return true
	}
	return false
}

// runStatusOf maps a final Session status onto the Run status.
func runStatusOf(s model.SessionStatus) model.RunStatus {
	switch s {
	case model.SessionCompleted:
		return model.RunSucceeded
	case model.SessionCancelled:
		return model.RunCancelled
	default:
		return model.RunFailed
	}
}

// hashText digests one prompt body.
func hashText(text string) string {
	return sha256Hex([]byte(text))
}

// hashInput digests the canonical serialization of one structured input.
// encoding/json emits map keys in sorted order, so the digest is
// deterministic (brief Step 5).
func hashInput(input any) (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", model.InvalidInputFault("structured input cannot be serialized")
	}
	return sha256Hex(data), nil
}
