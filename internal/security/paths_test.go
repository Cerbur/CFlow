package security_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// tempRoot returns a temporary directory whose full path is free of
// symlinks. On macOS, os.TempDir() lives under /var/folders and /var is a
// symlink to /private/var; the guard rejects any symlink component in a
// managed path, so tests resolve the root to its canonical location
// before building managed paths under it.
func tempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	return p
}

// requireFaultCode asserts err is a model.Fault carrying exactly code.
func requireFaultCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	if got, ok := model.CodeOf(err); !ok || got != code {
		t.Fatalf("expected fault %s, got %v", code, err)
	}
}

func mkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatalf("mkdirall %s: %v", path, err)
	}
}

// TestCheckHomeAcceptsFreshSafeHome: a freshly created 0700 directory
// owned by the effective user on the local filesystem passes the full
// owner, mode, filesystem, and advisory-lock probe (design 19.1).
func TestCheckHomeAcceptsFreshSafeHome(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)

	facts, err := security.CheckHome(security.HomeRequest{Path: home})
	if err != nil {
		t.Fatalf("safe home rejected: %v", err)
	}
	if facts.CanonicalPath != home {
		t.Fatalf("canonical path %q, want %q", facts.CanonicalPath, home)
	}
	if facts.Mode.Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", facts.Mode.Perm())
	}
	if facts.OwnerUID != os.Geteuid() || facts.EffectiveUID != os.Geteuid() {
		t.Fatalf("owner %d / effective %d, want %d", facts.OwnerUID, facts.EffectiveUID, os.Geteuid())
	}
	if facts.FileSystem == "" {
		t.Fatal("filesystem probe returned an empty name")
	}
	if !facts.AdvisoryLockOK {
		t.Fatal("advisory lock probe failed on the local filesystem")
	}
}

// TestCheckHomeRejectsExisting0755Home: an existing home with group or
// other permission bits fails closed and is never repaired (design 19.1).
func TestCheckHomeRejectsExisting0755Home(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := security.CheckHome(security.HomeRequest{Path: home})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)

	// The guard must not have repaired the mode.
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("guard modified the existing mode to %v", info.Mode().Perm())
	}
}

// TestCheckHomeRejectsGroupWritableParent: a parent directory writable by
// group or other users cannot prove path safety (a peer user could swap
// the directory), so the home fails closed.
func TestCheckHomeRejectsGroupWritableParent(t *testing.T) {
	root := tempRoot(t)
	parent := filepath.Join(root, "parent")
	mkdir(t, parent, 0o700)
	if err := os.Chmod(parent, 0o775); err != nil { // Chmod, not Mkdir: the umask would strip the group-write bit
		t.Fatalf("chmod: %v", err)
	}
	home := filepath.Join(parent, "home")
	mkdir(t, home, 0o700)

	_, err := security.CheckHome(security.HomeRequest{Path: home})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestCheckHomeRejectsSymlinkedHome: a home reached through a symlink is
// not its own canonical path and is rejected (symlink escape, design
// 19.1).
func TestCheckHomeRejectsSymlinkedHome(t *testing.T) {
	root := tempRoot(t)
	real := filepath.Join(root, "real")
	mkdir(t, real, 0o700)
	link := filepath.Join(root, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := security.CheckHome(security.HomeRequest{Path: link})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestCheckHomeRejectsSymlinkedAncestor: a symlink anywhere in the path
// above the home is rejected, even when the target itself is safe.
func TestCheckHomeRejectsSymlinkedAncestor(t *testing.T) {
	root := tempRoot(t)
	real := filepath.Join(root, "real")
	mkdir(t, real, 0o700)
	link := filepath.Join(root, "alink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	home := filepath.Join(link, "home")
	mkdir(t, home, 0o700)

	_, err := security.CheckHome(security.HomeRequest{Path: home})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestCheckHomeRejectsMissingOrNonDirectoryHome: the home must exist and
// be a directory.
func TestCheckHomeRejectsMissingOrNonDirectoryHome(t *testing.T) {
	root := tempRoot(t)
	missing := filepath.Join(root, "nope")
	if _, err := security.CheckHome(security.HomeRequest{Path: missing}); err == nil {
		t.Fatal("missing home accepted")
	}

	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := security.CheckHome(security.HomeRequest{Path: file}); err == nil {
		t.Fatal("file accepted as home")
	}
}

// TestCheckHomeRejectsRelativeOrEmptyPath: non-absolute requests are
// caller errors.
func TestCheckHomeRejectsRelativeOrEmptyPath(t *testing.T) {
	if _, err := security.CheckHome(security.HomeRequest{Path: ""}); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := security.CheckHome(security.HomeRequest{Path: "relative/home"}); err == nil {
		t.Fatal("relative path accepted")
	}
}

// TestCheckPathAcceptsManagedDirAndFile: managed 0700 directories and
// 0600 files inside a safe root report exact facts.
func TestCheckPathAcceptsManagedDirAndFile(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	data := filepath.Join(home, "data")
	mkdir(t, data, 0o700)
	file := filepath.Join(data, "run.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	facts, err := security.CheckPath(security.PathRequest{Path: data, Root: home, Kind: security.KindDir})
	if err != nil {
		t.Fatalf("managed dir rejected: %v", err)
	}
	if facts.CanonicalPath != data || facts.Kind != security.KindDir || facts.Mode.Perm() != 0o700 {
		t.Fatalf("unexpected dir facts: %+v", facts)
	}
	if !facts.InsideRoot || facts.IsRoot {
		t.Fatalf("containment facts wrong: %+v", facts)
	}

	facts, err = security.CheckPath(security.PathRequest{Path: file, Root: home, Kind: security.KindFile})
	if err != nil {
		t.Fatalf("managed file rejected: %v", err)
	}
	if facts.CanonicalPath != file || facts.Kind != security.KindFile || facts.Mode.Perm() != 0o600 {
		t.Fatalf("unexpected file facts: %+v", facts)
	}
	if facts.OwnerUID != os.Geteuid() {
		t.Fatalf("owner %d, want %d", facts.OwnerUID, os.Geteuid())
	}
}

// TestCheckPathRejectsSymlinkedParent: a symlink inside the managed tree
// is symlink traversal and fails closed.
func TestCheckPathRejectsSymlinkedParent(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	data := filepath.Join(home, "data")
	mkdir(t, data, 0o700)
	link := filepath.Join(home, "data-link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := security.CheckPath(security.PathRequest{Path: filepath.Join(link, "x.json"), Root: home, Kind: security.KindFile})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)

	// The final component itself must not be a symlink either.
	realFile := filepath.Join(home, "real.json")
	if err := os.WriteFile(realFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	linkFile := filepath.Join(home, "f.json")
	if err := os.Symlink(realFile, linkFile); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err = security.CheckPath(security.PathRequest{Path: linkFile, Root: home, Kind: security.KindFile})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestCheckPathRejectsTraversal: ".." components that escape the managed
// root fail containment (broad path, design 19.1).
func TestCheckPathRejectsTraversal(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)

	for _, path := range []string{
		filepath.Join(home, "..", "outside"),
		filepath.Join(home, "a", "..", "..", "outside"),
	} {
		_, err := security.CheckPath(security.PathRequest{Path: path, Root: home})
		requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
	}
}

// TestCheckPathRejectsRootAndHomeAsCleanupTargets: "/" is never inside a
// managed root, and the root itself is reported as IsRoot so cleanup
// logic can refuse to delete it. Creation at root or home always fails.
func TestCheckPathRejectsRootAndHomeAsCleanupTargets(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)

	if _, err := security.CheckPath(security.PathRequest{Path: "/", Root: home}); err == nil {
		t.Fatal("filesystem root accepted as a managed path")
	}

	facts, err := security.CheckPath(security.PathRequest{Path: home, Root: home})
	if err != nil {
		t.Fatalf("home itself rejected: %v", err)
	}
	if !facts.IsRoot || !facts.InsideRoot {
		t.Fatalf("home must be reported as the containment root, got %+v", facts)
	}

	if err := security.CreateSensitiveDir(home); err == nil {
		t.Fatal("created the home again")
	}
	if _, err := security.CreateSensitiveFile(home); err == nil {
		t.Fatal("created a file over the home")
	}
	// A system directory is never an acceptable parent: not owned by the
	// effective user (or not owner-only), so creation fails closed before
	// touching the filesystem.
	if _, err := security.CreateSensitiveFile(filepath.Join("/etc", "cflow-guard-probe")); err == nil {
		t.Fatal("created inside a system directory")
	}
}

// TestCheckPathRejectsWrongOwner: a path owned by another user fails the
// owner check. /etc/passwd is owned by root on every POSIX system; when
// the tests run as root it still fails the group/other mode check, so the
// test is valid under either euid.
func TestCheckPathRejectsWrongOwner(t *testing.T) {
	info, err := os.Lstat("/etc/passwd")
	if err != nil {
		t.Skipf("no /etc/passwd: %v", err)
	}
	if info.Mode().Perm()&0o077 == 0 && os.Geteuid() == 0 {
		t.Skip("running as root and /etc/passwd is owner-only: owner check not reachable")
	}
	_, err = security.CheckPath(security.PathRequest{Path: "/etc/passwd", Kind: security.KindFile})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestCheckPathRejectsGroupOrOtherPermissions: group or other permission
// bits on a managed path fail closed and are never repaired.
func TestCheckPathRejectsGroupOrOtherPermissions(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)

	looseDir := filepath.Join(home, "loose")
	mkdir(t, looseDir, 0o755)
	if _, err := security.CheckPath(security.PathRequest{Path: looseDir, Root: home, Kind: security.KindDir}); err == nil {
		t.Fatal("0755 dir accepted")
	}

	looseFile := filepath.Join(home, "loose.json")
	if err := os.WriteFile(looseFile, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := security.CheckPath(security.PathRequest{Path: looseFile, Root: home, Kind: security.KindFile}); err == nil {
		t.Fatal("0644 file accepted")
	}

	// The guard must not have repaired either mode.
	if fi, _ := os.Stat(looseDir); fi.Mode().Perm() != 0o755 {
		t.Fatal("guard repaired the loose directory mode")
	}
	if fi, _ := os.Stat(looseFile); fi.Mode().Perm() != 0o644 {
		t.Fatal("guard repaired the loose file mode")
	}
}

// TestCheckPathRejectsKindMismatch: the requested type must match the
// actual file type.
func TestCheckPathRejectsKindMismatch(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	file := filepath.Join(home, "f.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := security.CheckPath(security.PathRequest{Path: file, Root: home, Kind: security.KindDir}); err == nil {
		t.Fatal("file accepted as directory")
	}
	if _, err := security.CheckPath(security.PathRequest{Path: home, Root: home, Kind: security.KindFile}); err == nil {
		t.Fatal("directory accepted as file")
	}
}

// TestCheckPathRejectsUnexpectedFileType: sockets, FIFOs, and other
// non-regular, non-directory types are never managed paths.
func TestCheckPathRejectsUnexpectedFileType(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	fifo := filepath.Join(home, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := security.CheckPath(security.PathRequest{Path: fifo, Root: home}); err == nil {
		t.Fatal("FIFO accepted as a managed path")
	}
}

// TestCreateSensitiveFileBorn0600OExcl: new sensitive files are created
// with O_CREATE|O_EXCL and are born 0600; an existing path is never
// reused.
func TestCreateSensitiveFileBorn0600OExcl(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	path := filepath.Join(home, "artifact.json")

	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte("{}")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("born mode %v, want 0600", info.Mode().Perm())
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("created path is a symlink")
	}

	if _, err := security.CreateSensitiveFile(path); err == nil {
		t.Fatal("second create over an existing file succeeded")
	}
}

// TestCreateSensitiveDirBorn0700: new managed directories are born 0700
// and existing paths are never reused.
func TestCreateSensitiveDirBorn0700(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	dir := filepath.Join(home, "runs")

	if err := security.CreateSensitiveDir(dir); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("born mode %v, want 0700", info.Mode().Perm())
	}

	if err := security.CreateSensitiveDir(dir); err == nil {
		t.Fatal("second create over an existing dir succeeded")
	}
	if err := security.CreateSensitiveDir(filepath.Join(home, "missing", "child")); err == nil {
		t.Fatal("create succeeded with a missing parent")
	}
}

// TestCreateSensitiveFileRefusesSymlinkedParent: creation through a
// symlink inside the managed tree is refused (symlink traversal).
func TestCreateSensitiveFileRefusesSymlinkedParent(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	data := filepath.Join(home, "data")
	mkdir(t, data, 0o700)
	link := filepath.Join(home, "data-link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := security.CreateSensitiveFile(filepath.Join(link, "x.json")); err == nil {
		t.Fatal("created through a symlinked parent")
	}
}

// TestCreateSensitiveFileRefusesUnsafeParentMode: creation inside a
// parent with group or other permission bits is refused.
func TestCreateSensitiveFileRefusesUnsafeParentMode(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	loose := filepath.Join(home, "loose")
	mkdir(t, loose, 0o755)

	if _, err := security.CreateSensitiveFile(filepath.Join(loose, "x.json")); err == nil {
		t.Fatal("created inside an unsafe parent")
	}
}

// TestCheckPathAcceptsFileWithoutRoot: containment is optional; the
// remaining checks still apply.
func TestCheckPathAcceptsFileWithoutRoot(t *testing.T) {
	root := tempRoot(t)
	file := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	facts, err := security.CheckPath(security.PathRequest{Path: file, Kind: security.KindFile})
	if err != nil {
		t.Fatalf("file rejected: %v", err)
	}
	if facts.InsideRoot || facts.IsRoot {
		t.Fatalf("containment facts must be false without a root: %+v", facts)
	}
}

// ---------------------------------------------------------------------------
// the safe-cleanup scratch guard (design 17.4)
// ---------------------------------------------------------------------------

// TestCheckCleanupScratchAcceptsExactScratchUnderHome: an exact canonical
// scratch directory inside CFLOW_HOME passes the guard.
func TestCheckCleanupScratchAcceptsExactScratchUnderHome(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	scratch := filepath.Join(home, "runs", "run-1", "tmp")
	mkdirAll(t, scratch, 0o700)
	repo := filepath.Join(root, "repo")
	mkdir(t, repo, 0o700)

	facts, err := security.CheckCleanupScratch(security.CleanupScratchRequest{
		Path: scratch, HomeRoot: home, WorkspaceRoot: repo,
	})
	if err != nil {
		t.Fatalf("exact scratch rejected: %v", err)
	}
	if !facts.InsideRoot {
		t.Fatalf("exact scratch must resolve inside home")
	}
}

// TestCheckCleanupScratchRejectsRootsAndBroadAncestors: the filesystem
// root, `~`, an unresolved-variable token, the HomeRoot, the
// WorkspaceRoot, and a broad ancestor of either are never an exact scratch
// target.
func TestCheckCleanupScratchRejectsRootsAndBroadAncestors(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	repo := filepath.Join(root, "repo")
	mkdir(t, repo, 0o700)

	for _, bad := range []string{"", "/", "~", "$HOME/x", home, repo, filepath.Dir(repo)} {
		_, err := security.CheckCleanupScratch(security.CleanupScratchRequest{
			Path: bad, HomeRoot: home, WorkspaceRoot: repo,
		})
		if err == nil {
			t.Fatalf("scratch path %q must be rejected", bad)
		}
	}
}

// TestCheckCleanupScratchRejectsSymlinkEscape: a scratch target that
// resolves through a symlink (inside or outside) is never removed.
func TestCheckCleanupScratchRejectsSymlinkEscape(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	link := filepath.Join(home, "escape")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repo := filepath.Join(root, "repo")
	mkdir(t, repo, 0o700)
	_, err := security.CheckCleanupScratch(security.CleanupScratchRequest{
		Path: link, HomeRoot: home, WorkspaceRoot: repo,
	})
	if err == nil {
		t.Fatal("a symlink scratch target must be rejected")
	}
}

// TestCheckCleanupScratchRejectsWrongOwner: a target owned by another user
// fails the owner gate. /etc/passwd is root-owned on every POSIX system;
// when running as root the guard still fails closed.
func TestCheckCleanupScratchRejectsWrongOwner(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "cflow")
	mkdir(t, home, 0o700)
	if _, err := os.Lstat("/etc/passwd"); err != nil {
		t.Skipf("no /etc/passwd: %v", err)
	}
	repo := filepath.Join(root, "repo")
	mkdir(t, repo, 0o700)
	_, err := security.CheckCleanupScratch(security.CleanupScratchRequest{
		Path: "/etc/passwd", HomeRoot: home, WorkspaceRoot: repo,
	})
	if err == nil {
		t.Fatal("a non-owned scratch target must be rejected")
	}
}
