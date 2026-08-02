//go:build !darwin && !linux

package store

// Fallback platforms: no advisory flock is available. SQLite's own
// BEGIN IMMEDIATE serialization keeps a single migration transaction
// atomic, but cross-process migration exclusion on these platforms is not
// provided here; it arrives with Task 6's Schema Lock API.

type schemaLock struct{}

func acquireSchemaLock(string) (*schemaLock, error) { return &schemaLock{}, nil }

func (l *schemaLock) Close() error { return nil }
