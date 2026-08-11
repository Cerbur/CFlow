package tui

import (
	"fmt"
	"os"
	"path/filepath"

	"cflow.local/cflow/internal/security"
)

const operationLogName = "tui.jsonl"

func operationLogPath(home string) string {
	return filepath.Join(home, "logs", operationLogName)
}

// OpenOperationLog opens the process-wide TUI diagnostic log under
// <CFLOW_HOME>/logs/tui.jsonl. These records are non-authoritative
// breadcrumbs for GUI debugging; workflow events remain in the existing
// SQLite-backed and workflow-local audit paths.
//
// The managed directory and file are owner-only. A missing CFLOW_HOME is
// created during interactive TUI startup, while an existing unsafe path is
// rejected by the Security Guard.
func OpenOperationLog(home string) (*os.File, error) {
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return nil, fmt.Errorf("open TUI operation log: CFLOW_HOME must be an absolute clean path")
	}

	if _, err := os.Stat(home); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open TUI operation log: inspect CFLOW_HOME: %w", err)
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("open TUI operation log: create CFLOW_HOME: %w", err)
		}
	}
	if _, err := security.CheckPath(security.PathRequest{
		Path: home,
		Kind: security.KindDir,
	}); err != nil {
		return nil, fmt.Errorf("open TUI operation log: validate CFLOW_HOME: %w", err)
	}

	logsDir := filepath.Join(home, "logs")
	if _, err := os.Stat(logsDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open TUI operation log: inspect logs directory: %w", err)
		}
		if err := security.CreateSensitiveDir(logsDir); err != nil {
			// Another CFlow process may have won the create race. Recheck
			// the resulting path before returning the original failure.
			if _, statErr := os.Stat(logsDir); statErr != nil {
				return nil, fmt.Errorf("open TUI operation log: create logs directory: %w", err)
			}
		}
	}
	if _, err := security.CheckPath(security.PathRequest{
		Path: logsDir,
		Kind: security.KindDir,
	}); err != nil {
		return nil, fmt.Errorf("open TUI operation log: validate logs directory: %w", err)
	}

	path := operationLogPath(home)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		file, createErr := security.CreateSensitiveFile(path)
		if createErr == nil {
			return file, nil
		}
		// A concurrent process may have created the append-only log after
		// the first Stat. Revalidate before falling through to append mode.
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, fmt.Errorf("open TUI operation log: create log file: %w", createErr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("open TUI operation log: inspect log file: %w", err)
	}

	facts, err := security.CheckPath(security.PathRequest{
		Path: path,
		Kind: security.KindFile,
	})
	if err != nil {
		return nil, fmt.Errorf("open TUI operation log: validate log file: %w", err)
	}
	if facts.Mode.Perm() != 0o600 {
		return nil, fmt.Errorf("open TUI operation log: log file must have mode 0600")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open TUI operation log: append log file: %w", err)
	}
	return file, nil
}
