package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"cflow.local/cflow/internal/model"
)

// operationLogEntry is a bounded diagnostic breadcrumb, not authoritative
// Runtime state. It deliberately records types, identities, and outcomes
// rather than command arguments or user-entered text.
type operationLogEntry struct {
	At                string `json:"at"`
	OperationID       string `json:"operation_id,omitempty"`
	Workflow          string `json:"workflow,omitempty"`
	Page              string `json:"page,omitempty"`
	Kind              string `json:"kind"`
	Action            string `json:"action,omitempty"`
	Command           string `json:"command,omitempty"`
	Query             string `json:"query,omitempty"`
	Generation        uint64 `json:"generation,omitempty"`
	CommandGeneration uint64 `json:"command_generation,omitempty"`
	View              string `json:"view,omitempty"`
	Result            string `json:"result,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
}

type operationLogger struct {
	mu sync.Mutex
	w  io.Writer
}

func newOperationLogger(w io.Writer) *operationLogger {
	if w == nil {
		w = io.Discard
	}
	return &operationLogger{w: w}
}

func (l *operationLogger) write(entry operationLogEntry) {
	if l == nil {
		return
	}
	entry.At = time.Now().UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(body, '\n'))
}

func operationType(v any) string {
	return fmt.Sprintf("%T", v)
}

func operationErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if code, ok := model.CodeOf(err); ok {
		return string(code)
	}
	return "UNCLASSIFIED"
}

func operationResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func pageName(page Page) string {
	switch page {
	case PageWorkspace:
		return "workspace"
	case PageWorkflowMenu:
		return "workflow_menu"
	case PageReadonlyWorkspace:
		return "readonly_workspace"
	case PageActionPreview:
		return "action_preview"
	case PageCreatePreview:
		return "create_preview"
	case PageDiscussion:
		return "discussion"
	case PagePlanApproval:
		return "plan_approval"
	case PageExecutionApproval:
		return "execution_approval"
	case PageExecution:
		return "execution"
	case PageBlocked:
		return "blocked"
	case PageTerminal:
		return "terminal"
	case PageCreate:
		return "create"
	case PageCancel:
		return "cancel"
	case PagePauseExit:
		return "pause_exit"
	case PageMigration:
		return "migration"
	default:
		return "unknown"
	}
}
