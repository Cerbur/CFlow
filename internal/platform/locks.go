// Package platform implements the OS concurrency and process identity
// primitives (design 18, 13.2): the advisory LockSet in CFlow's fixed
// order and PID/start-token/process-group facts.
//
// The OS Advisory Lock is the live mutual-exclusion fact; SQLite Lease
// rows are diagnostics only and are written by the optional lease
// callback. A live lock is never stolen based on heartbeat age: only the
// operating system releases a lock, when its holder exits.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// LockKind is one level of the fixed lock order (design 18.1):
//
//	DB Schema Lock -> Project Writer -> Workflow Owner ->
//	Integration / Apply Lock -> lexicographically sorted Node Resource Locks
type LockKind int

const (
	// LockSchema is shared for normal DB use and exclusive for migration.
	LockSchema LockKind = iota + 1
	// LockProjectWriter permits only one mutating Runtime per Project.
	LockProjectWriter
	// LockWorkflowOwner identifies the foreground Runtime coordinating
	// a Workflow.
	LockWorkflowOwner
	// LockIntegration serializes changes to the trusted delivery chain.
	LockIntegration
	// LockResource prevents statically known Task conflicts inside an
	// approved DAG.
	LockResource
)

// order returns the position of the kind in the fixed lock order.
func (k LockKind) order() int {
	switch k {
	case LockSchema:
		return 1
	case LockProjectWriter:
		return 2
	case LockWorkflowOwner:
		return 3
	case LockIntegration:
		return 4
	case LockResource:
		return 5
	}
	return 0
}

// String renders the lock kind.
func (k LockKind) String() string {
	switch k {
	case LockSchema:
		return "schema"
	case LockProjectWriter:
		return "project-writer"
	case LockWorkflowOwner:
		return "workflow-owner"
	case LockIntegration:
		return "integration"
	case LockResource:
		return "resource"
	}
	return fmt.Sprintf("lock-kind(%d)", int(k))
}

// Errors the LockSet returns. Lock-order violations are programming
// errors; busy means the OS lock is held by another owner.
var (
	ErrLockOrderViolation = errors.New("platform: lock order violation")
	ErrLockBusy           = errors.New("platform: lock is held by another owner")
)

// LeaseState is the diagnostic lifecycle state of a lease record.
type LeaseState int

const (
	// LeaseAcquired records an acquisition.
	LeaseAcquired LeaseState = iota
	// LeaseReleased records a release.
	LeaseReleased
)

// Lease is the SQLite lease diagnostic row (design 18.2, PRD 决策 5):
// the OS lock is truth; the lease is CLI display, audit, and recovery
// metadata. Callbacks must not call back into the LockSet.
type Lease struct {
	Kind       LockKind
	Name       string
	PID        int
	StartToken uint64
	State      LeaseState
	Time       time.Time
}

// lockMode is the flock mode of one acquisition.
type lockMode int

const (
	modeShared lockMode = iota
	modeExclusive
)

// LockSet is the structured advisory-lock module. It exposes only
// acquisition methods that encode the fixed order; callers do not
// concatenate lock paths or acquire a lower-level lock directly.
// One LockSet should exist per process: it refcounts shared locks and
// tracks acquisition chains per goroutine.
type LockSet struct {
	dir       string
	onLease   func(Lease)
	selfPID   int
	selfToken uint64

	mu     sync.Mutex
	files  map[string]*lockFile // lock file name -> OS state
	chains map[uint64][]LockKind
}

// lockFile is the per-file OS lock state: one descriptor per file in
// this process, shared by refcount.
type lockFile struct {
	name   string
	f      *os.File
	shared bool
	refs   int
}

// Hold is one acquired lock or batch of locks. Release is idempotent.
type Hold struct {
	set   *LockSet
	kind  LockKind
	names []string
	gid   uint64
	held  bool
}

// Release drops the hold. Release order within a hold is irrelevant;
// the OS releases every lock the moment its process exits anyway.
func (h *Hold) Release() {
	if h == nil || !h.held {
		return
	}
	h.held = false
	h.set.release(h)
}

// OpenLockSet opens the lock directory (created 0700 when missing) and
// returns a LockSet for it. onLease, when non-nil, receives acquire and
// release diagnostics; it never gates the OS lock.
func OpenLockSet(dir string, onLease func(Lease)) (*LockSet, error) {
	if dir == "" {
		return nil, errors.New("platform: lock directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &LockSet{
		dir:     dir,
		onLease: onLease,
		files:   map[string]*lockFile{},
		chains:  map[uint64][]LockKind{},
	}
	if onLease != nil {
		pid, token, err := SelfIdentity()
		if err != nil {
			return nil, err
		}
		s.selfPID = pid
		s.selfToken = token
	}
	return s, nil
}

// SchemaShared takes the shared DB Schema Lock. Ordinary DB users hold
// it for the database lifetime; it is re-entrant and never conflicts
// with other shared holders, even across projects.
func (s *LockSet) SchemaShared(ctx context.Context) (*Hold, error) {
	return s.acquire(ctx, LockSchema, "", modeShared)
}

// SchemaExclusive takes the exclusive DB Schema Lock for migration. It
// is rejected while any shared holder is active in this process, and the
// OS lock excludes other processes.
func (s *LockSet) SchemaExclusive(ctx context.Context) (*Hold, error) {
	return s.acquire(ctx, LockSchema, "", modeExclusive)
}

// ProjectWriter takes the writer lock of one Project: only one mutating
// Runtime per Project.
func (s *LockSet) ProjectWriter(ctx context.Context, project string) (*Hold, error) {
	if err := validateName(LockProjectWriter, project); err != nil {
		return nil, err
	}
	return s.acquire(ctx, LockProjectWriter, project, modeExclusive)
}

// WorkflowOwner takes the owner lock identifying the foreground Runtime
// coordinating one Workflow of a Project.
func (s *LockSet) WorkflowOwner(ctx context.Context, project, workflow string) (*Hold, error) {
	if err := validateName(LockWorkflowOwner, project); err != nil {
		return nil, err
	}
	if err := validateName(LockWorkflowOwner, workflow); err != nil {
		return nil, err
	}
	return s.acquire(ctx, LockWorkflowOwner, project+"/"+workflow, modeExclusive)
}

// Integration takes the Integration/Apply Lock serializing changes to
// the trusted delivery chain of one Project.
func (s *LockSet) Integration(ctx context.Context, project string) (*Hold, error) {
	if err := validateName(LockIntegration, project); err != nil {
		return nil, err
	}
	return s.acquire(ctx, LockIntegration, project, modeExclusive)
}

// Resource takes Node Resource Locks for one batch, in lexicographic
// name order. One Hold releases the whole batch.
func (s *LockSet) Resource(ctx context.Context, names ...string) (*Hold, error) {
	if len(names) == 0 {
		return nil, errors.New("platform: at least one resource name is required")
	}
	// Resource Locks are acquired in lexicographic order (design 18.1).
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	uniq := make([]string, 0, len(sorted))
	var prev string
	for _, n := range sorted {
		if err := validateName(LockResource, n); err != nil {
			return nil, err
		}
		if n == prev {
			continue
		}
		prev = n
		uniq = append(uniq, n)
	}
	var holds []*Hold
	for _, n := range uniq {
		h, err := s.acquire(ctx, LockResource, n, modeExclusive)
		if err != nil {
			for _, held := range holds {
				held.Release()
			}
			return nil, err
		}
		holds = append(holds, h)
	}
	out := &Hold{set: s, kind: LockResource, gid: holds[0].gid, held: true}
	for _, h := range holds {
		h.held = false
		out.names = append(out.names, h.names...)
	}
	return out, nil
}

// LockPath reports the lock file path for kind and name. It is a
// diagnostics aid (doctor output); callers never open or flock the file
// themselves.
func (s *LockSet) LockPath(kind LockKind, name string) (string, error) {
	if err := validateName(kind, name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, lockFileName(kind, name)), nil
}

// validateName rejects empty caller names and unknown kinds.
func validateName(kind LockKind, name string) error {
	if kind < LockSchema || kind > LockResource {
		return fmt.Errorf("platform: unknown lock kind %d", int(kind))
	}
	if kind != LockSchema && name == "" {
		return fmt.Errorf("platform: %s requires a non-empty name", kind)
	}
	return nil
}

// acquire takes one OS advisory lock, enforcing the fixed order per
// goroutine chain. Acquisition blocks until the lock is free or ctx
// expires; the OS lock is the live mutual-exclusion fact.
func (s *LockSet) acquire(ctx context.Context, kind LockKind, name string, mode lockMode) (*Hold, error) {
	file := lockFileName(kind, name)
	gid := goroutineID()

	s.mu.Lock()
	chain := s.chains[gid]
	maxOrder := 0
	for _, k := range chain {
		if k.order() > maxOrder {
			maxOrder = k.order()
		}
	}
	if kind.order() < maxOrder {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s while holding a higher-order lock", ErrLockOrderViolation, kind)
	}
	// Same-process conflicts are rejected immediately: a second writer
	// for one project, or migration while a shared holder is active.
	if lf, ok := s.files[file]; ok {
		if lf.shared && mode == modeShared {
			lf.refs++
			s.chains[gid] = append(chain, kind)
			s.mu.Unlock()
			s.lease(kind, name, LeaseAcquired)
			return &Hold{set: s, kind: kind, names: []string{name}, gid: gid, held: true}, nil
		}
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s %q is already held in this process", ErrLockBusy, kind, name)
	}
	s.mu.Unlock()

	f, err := openLockFile(filepath.Join(s.dir, file))
	if err != nil {
		return nil, err
	}
	flag := unix.LOCK_EX
	if mode == modeShared {
		flag = unix.LOCK_SH
	}
	for {
		if err := unix.Flock(int(f.Fd()), flag|unix.LOCK_NB); err == nil {
			break
		} else if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Register the OS-held lock. A concurrent same-process acquirer may
	// have registered first: shared locks are then refcounted onto the
	// first descriptor; exclusive conflicts release the extra flock.
	s.mu.Lock()
	if lf, ok := s.files[file]; ok {
		if lf.shared && mode == modeShared {
			lf.refs++
			s.chains[gid] = append(s.chains[gid], kind)
			s.mu.Unlock()
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
			s.lease(kind, name, LeaseAcquired)
			return &Hold{set: s, kind: kind, names: []string{name}, gid: gid, held: true}, nil
		}
		s.mu.Unlock()
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s %q is already held in this process", ErrLockBusy, kind, name)
	}
	s.files[file] = &lockFile{name: file, f: f, shared: mode == modeShared, refs: 1}
	s.chains[gid] = append(s.chains[gid], kind)
	s.mu.Unlock()
	s.lease(kind, name, LeaseAcquired)
	return &Hold{set: s, kind: kind, names: []string{name}, gid: gid, held: true}, nil
}

// release drops every lock of the hold, in reverse acquisition order,
// and emits the release diagnostics.
func (s *LockSet) release(h *Hold) {
	type leaseRecord struct {
		kind LockKind
		name string
	}
	var records []leaseRecord
	s.mu.Lock()
	for i := len(h.names) - 1; i >= 0; i-- {
		name := h.names[i]
		file := lockFileName(h.kind, name)
		lf, ok := s.files[file]
		if !ok {
			continue
		}
		lf.refs--
		if lf.refs <= 0 {
			_ = unix.Flock(int(lf.f.Fd()), unix.LOCK_UN)
			_ = lf.f.Close()
			delete(s.files, file)
		}
		chain := s.chains[h.gid]
		for j := len(chain) - 1; j >= 0; j-- {
			if chain[j] == h.kind {
				s.chains[h.gid] = append(chain[:j], chain[j+1:]...)
				break
			}
		}
		records = append(records, leaseRecord{kind: h.kind, name: name})
	}
	s.mu.Unlock()
	for _, r := range records {
		s.lease(r.kind, r.name, LeaseReleased)
	}
}

// lease emits one diagnostic record. Errors cannot reach the lock path:
// lease rows are diagnostics only (design 18.2).
func (s *LockSet) lease(kind LockKind, name string, state LeaseState) {
	if s.onLease == nil {
		return
	}
	s.onLease(Lease{
		Kind:       kind,
		Name:       name,
		PID:        s.selfPID,
		StartToken: s.selfToken,
		State:      state,
		Time:       time.Now(),
	})
}

// lockFileName renders the lock file name for one kind and name. The
// segment encoding is injective: distinct names never collide.
func lockFileName(kind LockKind, name string) string {
	prefix := "schema"
	switch kind {
	case LockProjectWriter:
		prefix = "writer"
	case LockWorkflowOwner:
		prefix = "owner"
	case LockIntegration:
		prefix = "integration"
	case LockResource:
		prefix = "resource"
	}
	if kind == LockSchema {
		return prefix + ".lock"
	}
	return prefix + "-" + safeLockSegment(name) + ".lock"
}

// safeLockSegment maps a caller-provided name to a filename-safe
// segment. Literal '_' becomes "__"; every other non-alphanumeric byte
// becomes "_HH" (uppercase hex). The encoding is a prefix code, so it
// is injective: distinct names never share a segment.
func safeLockSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		case c == '_':
			b.WriteString("__")
		default:
			fmt.Fprintf(&b, "_%02X", c)
		}
	}
	return b.String()
}

// openLockFile opens a lock file 0600 with O_CLOEXEC: the descriptor
// can never be inherited by a supervised child (design 13.2).
func openLockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// goroutineID returns the current goroutine's ID for per-chain lock
// order tracking. It parses the runtime stack header
// ("goroutine 123 [running]:"); a parse failure falls back to 0, which
// conservatively shares one chain.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := string(buf[:n])
	start := strings.IndexByte(s, ' ')
	if start < 0 {
		return 0
	}
	end := strings.IndexByte(s[start+1:], ' ')
	if end < 0 {
		return 0
	}
	id, err := strconv.ParseUint(s[start+1:start+1+end], 10, 64)
	if err != nil {
		return 0
	}
	return id
}
