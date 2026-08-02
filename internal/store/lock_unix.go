//go:build darwin || linux

package store

// The exclusive DB Schema Lock migration needs while it upgrades the
// database (design 9.3, PRD 决策 3): ordinary users hold the shared Schema
// Lock for the database lifetime, migration holds the exclusive lock while
// it re-reads, backs up, and migrates. The formal lock API and the shared
// hold arrive with Task 6 (internal/platform); until then this minimal
// flock provides the cross-process exclusion that keeps concurrent
// migration exactly-once. The lock file lives next to the database, which
// PRD 决策 5 requires to be on a local filesystem with reliable advisory
// locks (doctor verifies this in Task 3).

import (
	"os"

	"golang.org/x/sys/unix"
)

// schemaLock is the exclusive migration lock.
type schemaLock struct {
	f *os.File
}

// acquireSchemaLock takes the exclusive Schema Lock for the database at
// path. It blocks until the lock is free.
func acquireSchemaLock(lockPath string) (*schemaLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &schemaLock{f: f}, nil
}

// Close releases the lock.
func (l *schemaLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}
