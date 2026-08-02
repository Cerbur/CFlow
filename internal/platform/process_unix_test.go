package platform_test

// Platform process identity integration tests (design 13.2, 22.1) with
// real processes: the OS start token, process-group signaling through
// the Supervisor seam, lock-descriptor non-inheritance, and force-kill
// escalation. Helper processes are the re-exec'd test binary dispatched
// by TestMain in the platform package (CFLOW_TEST_HELPER roles).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/process"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// startHelper spawns this test binary as a CFLOW_TEST_HELPER role.
func startHelper(t *testing.T, role string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	requireNoError(t, err)
	cmd := exec.Command(exe)
	cmd.Env = []string{"CFLOW_TEST_HELPER=" + role}
	return cmd
}

// specForHelper builds a bounded, argv-only ProcessSpec that re-execs
// this test binary as a CFLOW_TEST_HELPER role.
func specForHelper(role string, extraEnv map[string]string) process.ProcessSpec {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	env := map[string]string{"CFLOW_TEST_HELPER": role}
	for k, v := range extraEnv {
		env[k] = v
	}
	return process.ProcessSpec{
		Executable:     exe,
		Env:            env,
		MaxFrameBytes:  1 << 20,
		MaxOutputBytes: 1 << 20,
	}
}

// waitGone polls until every PID reports dead (reaped).
func waitGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, pid := range pids {
			if platform.Alive(pid) {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("processes still alive: %v", pids)
}

// TestPIDStartTokenMatchesLiveProcess asserts the platform start token
// identifies a live process, dies with it, and ProcessGroup reports the
// real group.
func TestPIDStartTokenMatchesLiveProcess(t *testing.T) {
	cmd := startHelper(t, "child")
	requireNoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	token, err := platform.StartToken(cmd.Process.Pid)
	requireNoError(t, err)
	if token == 0 {
		t.Fatal("start token is zero for a live process")
	}
	pgid, err := platform.ProcessGroup(cmd.Process.Pid)
	requireNoError(t, err)
	if pgid <= 0 {
		t.Fatalf("process group=%d, want > 0", pgid)
	}
	if !platform.Alive(cmd.Process.Pid) {
		t.Fatal("Alive=false for a live process")
	}
	requireNoError(t, cmd.Process.Kill())
	_ = cmd.Wait() // a killed helper exits by signal; the fact is the death itself
	if platform.Alive(cmd.Process.Pid) {
		t.Fatal("Alive=true after kill and reap")
	}
	if _, err := platform.StartToken(cmd.Process.Pid); err == nil {
		t.Fatal("start token readable for a dead process")
	}
}

// TestPIDGroupSignalReachesChildren asserts the Supervisor terminates the
// exact process group: a real parent and its real child, both outside the
// test's own group, die from one Terminate.
func TestPIDGroupSignalReachesChildren(t *testing.T) {
	supervisor := process.NewSupervisor(process.NewOSAdapter())
	h, events, err := supervisor.Start(context.Background(), specForHelper("parent", nil))
	requireNoError(t, err)

	var childPID int
	for ev := range events {
		if ev.Kind == process.EventFrameOut {
			var info struct {
				Parent int `json:"parent"`
				Child  int `json:"child"`
			}
			if err := json.Unmarshal(ev.Frame, &info); err != nil {
				t.Fatalf("helper line %q: %v", ev.Frame, err)
			}
			childPID = info.Child
			break
		}
	}
	if childPID == 0 {
		t.Fatal("helper reported no child PID")
	}
	if !platform.Alive(childPID) {
		t.Fatal("child not alive after start")
	}

	requireNoError(t, supervisor.Signal(context.Background(), h, process.Terminate))
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Fact != process.FactSignaled || exit.Signal != 15 {
		t.Fatalf("exit=%+v, want FactSignaled by SIGTERM", exit)
	}
	waitGone(t, childPID)
	for range events {
	}
}

// TestPIDForceKillEscalation asserts the primitives of the controlled
// stop protocol (design 13.3): a leader that traps Terminate survives it
// and stays inspectable with its identity, and ForceKill takes the group
// down.
func TestPIDForceKillEscalation(t *testing.T) {
	supervisor := process.NewSupervisor(process.NewOSAdapter())
	h, events, err := supervisor.Start(context.Background(), specForHelper("trap", nil))
	requireNoError(t, err)

	var identity process.ProcessIdentity
	ready := false
	for ev := range events {
		switch ev.Kind {
		case process.EventStarted:
			identity = ev.Identity
		case process.EventFrameOut:
			if strings.TrimSpace(string(ev.Frame)) == "ready" {
				ready = true
			}
		}
		if ready {
			break
		}
	}
	if !ready {
		t.Fatal("trap helper never reported ready")
	}

	requireNoError(t, supervisor.Signal(context.Background(), h, process.Terminate))
	fact, err := supervisor.Inspect(context.Background(), identity)
	requireNoError(t, err)
	if !fact.Running {
		t.Fatalf("trapped leader not running after Terminate: %+v", fact)
	}

	requireNoError(t, supervisor.Signal(context.Background(), h, process.ForceKill))
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Fact != process.FactSignaled || exit.Signal != 9 {
		t.Fatalf("exit=%+v, want FactSignaled by SIGKILL", exit)
	}
	fact, err = supervisor.Inspect(context.Background(), identity)
	requireNoError(t, err)
	if fact.Running {
		t.Fatalf("leader still running after ForceKill: %+v", fact)
	}
	for range events {
	}
}

// TestPIDLockDescriptorNotInherited asserts CFlow lock descriptors never
// cross exec into a supervised child: a probe process that can flock an
// inherited descriptor would report it, and exits 0 when the exec was
// clean.
func TestPIDLockDescriptorNotInherited(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	locks, err := platform.OpenLockSet(dir, nil)
	requireNoError(t, err)
	excl, err := locks.SchemaExclusive(context.Background())
	requireNoError(t, err)
	defer excl.Release()
	lockFile, err := locks.LockPath(platform.LockSchema, "")
	requireNoError(t, err)

	supervisor := process.NewSupervisor(process.NewOSAdapter())
	h, events, err := supervisor.Start(context.Background(), specForHelper("lockprobe", map[string]string{"LOCK_FILE": lockFile}))
	requireNoError(t, err)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Code != 0 {
		var msgs []string
		for ev := range events {
			if ev.Kind == process.EventFrameOut || ev.Kind == process.EventFrameErr {
				msgs = append(msgs, string(ev.Frame))
			}
		}
		t.Fatalf("lock descriptor inherited: exit=%+v output=%v", exit, msgs)
	}
	for range events {
	}
}

// TestPIDExecFailureSurfacesAtStart asserts a missing executable fails
// Start synchronously; no process is ever supervised.
func TestPIDExecFailureSurfacesAtStart(t *testing.T) {
	supervisor := process.NewSupervisor(process.NewOSAdapter())
	spec := specForHelper("child", nil)
	spec.Executable = "/nonexistent/cflow-helper-binary"
	if _, _, err := supervisor.Start(context.Background(), spec); err == nil {
		t.Fatal("expected Start to fail for a missing executable")
	}
}
