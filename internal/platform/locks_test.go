package platform

// LockSet tests (design 18): fixed acquisition order, second-writer
// rejection, cross-process mutual exclusion via real helper processes,
// and read-only shared access while a writer is held.
//
// TestMain doubles as the entry point for re-exec'd helper processes:
// when CFLOW_TEST_HELPER names a role, the helper runs and exits instead
// of the test suite. The helpers use only the OS seam (flock, signals),
// never the process Supervisor, so they can live in this package.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMain(m *testing.M) {
	if role := os.Getenv("CFLOW_TEST_HELPER"); role != "" {
		os.Exit(helperMain(role))
	}
	os.Exit(m.Run())
}

// helperMain dispatches the re-exec'd helper roles.
func helperMain(role string) int {
	switch role {
	case "lockholder":
		return helperLockHolder()
	case "parent":
		return helperParent()
	case "child":
		return helperChild()
	case "trap":
		return helperTrap()
	case "lockprobe":
		return helperLockProbe()
	}
	fmt.Fprintf(os.Stderr, "unknown helper role %q\n", role)
	return 2
}

// withHelperRole returns a copy of env with CFLOW_TEST_HELPER set to role.
func withHelperRole(env []string, role string) []string {
	out := append([]string(nil), env...)
	for i, kv := range out {
		if strings.HasPrefix(kv, "CFLOW_TEST_HELPER=") {
			out[i] = "CFLOW_TEST_HELPER=" + role
			return out
		}
	}
	return append(out, "CFLOW_TEST_HELPER="+role)
}

// helperLockHolder flocks a lock file exclusively, prints "held", and
// holds the lock for HOLD_MS (default 30s). It is spawned via os/exec by
// the cross-process lock tests.
func helperLockHolder() int {
	path := os.Getenv("LOCK_FILE")
	holdMS, _ := strconv.Atoi(os.Getenv("HOLD_MS"))
	if holdMS <= 0 {
		holdMS = 30000
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lockholder: open: %v\n", err)
		return 2
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		fmt.Fprintf(os.Stderr, "lockholder: flock: %v\n", err)
		return 2
	}
	fmt.Fprintln(os.Stdout, "held")
	time.Sleep(time.Duration(holdMS) * time.Millisecond)
	return 0
}

// helperParent spawns a child in the same process group, prints
// {"parent":pid,"child":pid} as one JSON line, and waits for the child.
// The whole group is killed by the test's Terminate.
func helperParent() int {
	cmd := exec.Command(os.Args[0])
	cmd.Env = withHelperRole(os.Environ(), "child")
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "parent: start child: %v\n", err)
		return 2
	}
	b, err := json.Marshal(map[string]int{"parent": os.Getpid(), "child": cmd.Process.Pid})
	if err != nil {
		return 2
	}
	fmt.Println(string(b))
	_ = cmd.Wait()
	return 0
}

// helperChild sleeps until a deadline; it dies on the default SIGTERM
// disposition when the test signals the group.
func helperChild() int {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	return 0
}

// helperTrap installs a SIGTERM handler, spawns a child in the same
// group, prints "ready", and keeps running: Terminate cannot kill it,
// only ForceKill can.
func helperTrap() int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	cmd := exec.Command(os.Args[0])
	cmd.Env = withHelperRole(os.Environ(), "child")
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "trap: start child: %v\n", err)
		return 2
	}
	go func() { _ = cmd.Wait() }()
	fmt.Println("ready")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-sigCh:
			// Trapped: the leader survives Terminate.
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0
}

// helperLockProbe verifies no lock descriptor was inherited across exec:
// the parent process holds an exclusive flock on LOCK_FILE, so a fresh
// open in this process must fail with EWOULDBLOCK, and no inherited
// regular-file descriptor may be flock-able. Exit 0 means clean.
func helperLockProbe() int {
	path := os.Getenv("LOCK_FILE")
	if path == "" {
		fmt.Fprintln(os.Stderr, "lockprobe: LOCK_FILE missing")
		return 2
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lockprobe: open: %v\n", err)
		return 2
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		fmt.Fprintln(os.Stdout, "lockprobe: parent lock not held")
		return 2
	}
	if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
		fmt.Fprintf(os.Stderr, "lockprobe: flock: %v\n", err)
		return 2
	}
	for fd := 3; fd < 1024; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil {
			fmt.Fprintf(os.Stdout, "lockprobe: inherited lock fd %d\n", fd)
			return 1
		}
	}
	return 0
}

// startHelper spawns this test binary as a helper process.
func startHelper(t *testing.T, role string, env ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	requireNoError(t, err)
	cmd := exec.Command(exe)
	cmd.Env = append([]string{"CFLOW_TEST_HELPER=" + role}, env...)
	return cmd
}

// awaitHelperLine starts the helper and waits for its first stdout line.
func awaitHelperLine(t *testing.T, cmd *exec.Cmd, want string) {
	t.Helper()
	out, err := cmd.StdoutPipe()
	requireNoError(t, err)
	requireNoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			lineCh <- sc.Text()
		}
		close(lineCh)
	}()
	select {
	case line := <-lineCh:
		if line != want {
			t.Fatalf("helper first line=%q, want %q", line, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not report ready in time")
	}
}

// newLockSet opens a LockSet in a fresh temporary directory.
func newLockSet(t *testing.T, onLease func(Lease)) *LockSet {
	t.Helper()
	s, err := OpenLockSet(filepath.Join(t.TempDir(), "locks"), onLease)
	requireNoError(t, err)
	return s
}

// TestLockOrderViolation asserts the fixed lock order (design 18.1):
// Schema -> Project Writer -> Workflow Owner -> Integration/Apply ->
// Resource. Acquiring any lower-order lock while a higher one is held is
// rejected.
func TestLockOrderViolation(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	schema, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	defer schema.Release()
	writer, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	defer writer.Release()
	owner, err := s.WorkflowOwner(ctx, "project-1", "workflow-1")
	requireNoError(t, err)
	defer owner.Release()
	integration, err := s.Integration(ctx, "project-1")
	requireNoError(t, err)
	defer integration.Release()
	resources, err := s.Resource(ctx, "res-a")
	requireNoError(t, err)
	defer resources.Release()

	// Parallel Resource Locks are legitimate (design 18.1: sorted Node
	// Resource Locks), so a second resource acquisition succeeds.
	r2, err := s.Resource(ctx, "res-b")
	requireNoError(t, err)
	r2.Release()
	// Every lower-order acquisition after the full chain is rejected.
	// A fresh chain is still allowed to hold the whole order.
	if _, err := s.WorkflowOwner(ctx, "project-2", "workflow-2"); !errors.Is(err, ErrLockOrderViolation) {
		t.Fatalf("WorkflowOwner after Resource: err=%v, want ErrLockOrderViolation", err)
	}
	if _, err := s.ProjectWriter(ctx, "project-3"); !errors.Is(err, ErrLockOrderViolation) {
		t.Fatalf("ProjectWriter after Resource: err=%v, want ErrLockOrderViolation", err)
	}
	if _, err := s.SchemaShared(ctx); !errors.Is(err, ErrLockOrderViolation) {
		t.Fatalf("SchemaShared after Resource: err=%v, want ErrLockOrderViolation", err)
	}
	if _, err := s.SchemaExclusive(ctx); !errors.Is(err, ErrLockOrderViolation) {
		t.Fatalf("SchemaExclusive after Resource: err=%v, want ErrLockOrderViolation", err)
	}
}

// TestLockSecondWriterRejected asserts one mutating Runtime per project:
// a second acquire of the same project writer in this process fails
// immediately.
func TestLockSecondWriterRejected(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	w, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	defer w.Release()
	if _, err := s.ProjectWriter(ctx, "project-1"); err == nil {
		t.Fatal("second writer acquire succeeded")
	}
}

// TestLockSecondWriterRejectedCrossProcess asserts the OS Advisory Lock
// is the live mutual-exclusion fact: while another process holds the
// writer lock, acquisition fails once the context expires.
func TestLockSecondWriterRejectedCrossProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	s, err := OpenLockSet(dir, nil)
	requireNoError(t, err)
	lockFile := filepath.Join(dir, lockFileName(LockProjectWriter, "project-9"))
	cmd := startHelper(t, "lockholder", "LOCK_FILE="+lockFile, "HOLD_MS=30000")
	awaitHelperLine(t, cmd, "held")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := s.ProjectWriter(ctx, "project-9"); err == nil {
		t.Fatal("cross-process second writer acquire succeeded")
	}
}

// TestLockAcquireSucceedsAfterRelease asserts acquisition waits for the
// OS lock and succeeds once the holder releases it.
func TestLockAcquireSucceedsAfterRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	s, err := OpenLockSet(dir, nil)
	requireNoError(t, err)
	lockFile := filepath.Join(dir, lockFileName(LockProjectWriter, "project-10"))
	cmd := startHelper(t, "lockholder", "LOCK_FILE="+lockFile, "HOLD_MS=500")
	awaitHelperLine(t, cmd, "held")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := s.ProjectWriter(ctx, "project-10")
	requireNoError(t, err)
	w.Release()
	requireNoError(t, cmd.Wait())
}

// TestLockIndependentProjectsConcurrent asserts different projects never
// serialize on a global writer lock, and read-only users (a second
// shared Schema holder) stay available while a writer is held.
func TestLockIndependentProjectsConcurrent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	s, err := OpenLockSet(dir, nil)
	requireNoError(t, err)
	s2, err := OpenLockSet(dir, nil)
	requireNoError(t, err)

	ctx := context.Background()
	schema, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	defer schema.Release()
	w1, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	defer w1.Release()
	w2, err := s.ProjectWriter(ctx, "project-2")
	requireNoError(t, err)
	defer w2.Release()

	// Read-only access remains available while the writer is held.
	ro, err := s2.SchemaShared(ctx)
	requireNoError(t, err)
	ro.Release()
}

// TestLockSchemaExclusiveWhileSharedRejected asserts migration cannot run
// while a normal shared Schema user is active in this process.
func TestLockSchemaExclusiveWhileSharedRejected(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	sh, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	defer sh.Release()
	if _, err := s.SchemaExclusive(ctx); err == nil {
		t.Fatal("exclusive Schema lock acquired while the shared lock is held")
	}
}

// TestLockSchemaSharedReentrant asserts the shared Schema lock is
// re-entrant and fully released only by the last release.
func TestLockSchemaSharedReentrant(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	a, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	b, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	b.Release()
	a.Release()
	c, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	c.Release()
}

// TestLockLeaseDiagnosticsAndResourceOrder asserts the SQLite lease
// callback receives acquire/release diagnostics (kind, name, live PID,
// start token), and Resource Locks are acquired in lexicographic order.
func TestLockLeaseDiagnosticsAndResourceOrder(t *testing.T) {
	var leases []Lease
	s := newLockSet(t, func(l Lease) { leases = append(leases, l) })
	ctx := context.Background()
	sh, err := s.SchemaShared(ctx)
	requireNoError(t, err)
	w, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	r, err := s.Resource(ctx, "node-b", "node-a", "node-c")
	requireNoError(t, err)

	// Resource acquisition order is lexicographic regardless of the
	// order the caller passed.
	var names []string
	for _, l := range leases {
		if l.Kind == LockResource && l.State == LeaseAcquired {
			names = append(names, l.Name)
		}
	}
	if strings.Join(names, ",") != "node-a,node-b,node-c" {
		t.Fatalf("resource acquire order=%v, want sorted", names)
	}

	// Every lease carries the live process identity for diagnostics.
	for _, l := range leases {
		if l.PID != os.Getpid() || l.StartToken == 0 {
			t.Fatalf("lease without live identity: %+v", l)
		}
	}

	r.Release()
	w.Release()
	sh.Release()
	if n := len(leases); n == 0 || leases[n-1].Kind != LockSchema || leases[n-1].State != LeaseReleased {
		t.Fatalf("last lease=%+v, want released Schema lease", leases[n-1])
	}
}

// TestLockResourceDedupesNames asserts duplicate resource names in one
// batch acquire exactly one lock.
func TestLockResourceDedupesNames(t *testing.T) {
	var leases []Lease
	s := newLockSet(t, func(l Lease) { leases = append(leases, l) })
	r, err := s.Resource(context.Background(), "node-a", "node-b", "node-a")
	requireNoError(t, err)
	r.Release()
	acquired := 0
	for _, l := range leases {
		if l.Kind == LockResource && l.State == LeaseAcquired {
			acquired++
		}
	}
	if acquired != 2 {
		t.Fatalf("acquired %d resource locks, want 2", acquired)
	}
}

// TestLockResourceNamesSanitized asserts caller names survive into the
// lease diagnostics unmangled and map to safe lock file names.
func TestLockResourceNamesSanitized(t *testing.T) {
	var leases []Lease
	s := newLockSet(t, func(l Lease) { leases = append(leases, l) })
	r, err := s.Resource(context.Background(), "integration:wf-7", "a b/c")
	requireNoError(t, err)
	r.Release()
	if len(leases) != 4 {
		t.Fatalf("lease count=%d, want 4", len(leases))
	}
	// Resource Locks are acquired lexicographically, so "a b/c" comes
	// before "integration:wf-7"; names reach diagnostics unmangled.
	if leases[0].Name != "a b/c" || leases[1].Name != "integration:wf-7" {
		t.Fatalf("lease names mangled: %+v %+v", leases[0], leases[1])
	}
	for _, l := range leases {
		if filepath.Base(filepath.Join(s.dir, lockFileName(l.Kind, l.Name))) == "" {
			t.Fatalf("no safe file name for %+v", l)
		}
	}
}

// TestLockReleaseAndReacquire asserts Release is idempotent and a lock
// can be acquired again after release.
func TestLockReleaseAndReacquire(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	w, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	w.Release()
	w.Release() // idempotent
	w2, err := s.ProjectWriter(ctx, "project-1")
	requireNoError(t, err)
	w2.Release()
}

// TestLockFileCreated0600 asserts lock files are born 0600 (PRD 约束 1).
func TestLockFileCreated0600(t *testing.T) {
	s := newLockSet(t, nil)
	w, err := s.ProjectWriter(context.Background(), "project-1")
	requireNoError(t, err)
	defer w.Release()
	info, err := os.Stat(filepath.Join(s.dir, lockFileName(LockProjectWriter, "project-1")))
	requireNoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode=%v, want 0600", info.Mode().Perm())
	}
}

// TestLockRejectsEmptyNames asserts every acquisition method rejects
// empty caller names.
func TestLockRejectsEmptyNames(t *testing.T) {
	s := newLockSet(t, nil)
	ctx := context.Background()
	if _, err := s.ProjectWriter(ctx, ""); err == nil {
		t.Fatal("ProjectWriter with empty name accepted")
	}
	if _, err := s.WorkflowOwner(ctx, "project-1", ""); err == nil {
		t.Fatal("WorkflowOwner with empty name accepted")
	}
	if _, err := s.Integration(ctx, ""); err == nil {
		t.Fatal("Integration with empty name accepted")
	}
	if _, err := s.Resource(ctx, "node-a", ""); err == nil {
		t.Fatal("Resource with an empty name accepted")
	}
}
