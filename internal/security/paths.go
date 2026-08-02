// Package security implements the Security Guard: owner-only path and
// permission validation for every managed location, plus the streaming
// versioned redaction policy (design 19). It never repairs an existing
// mode, never follows a symlink into or out of the managed tree, and
// never returns raw file contents in fault text.
//
// Path model: a managed path must be absolute, must be identical to its
// own canonical (EvalSymlinks) form, and every component must be a real
// directory with no group- or other-writable ancestor. Group or other
// permission bits on the path itself, a foreign owner, an unexpected
// file type, or a location outside the containment root fail closed with
// INSECURE_CFLOW_HOME_PERMISSIONS (PRD 约束 2). Files and directories
// created by CFlow are born 0600 and 0700 respectively via O_CREATE|O_EXCL
// semantics; an existing path is never reused.
package security

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
)

// Kind is the expected file type of a managed path.
type Kind string

const (
	// KindAny accepts a directory or a regular file.
	KindAny Kind = "any"
	// KindDir requires a directory.
	KindDir Kind = "dir"
	// KindFile requires a regular file.
	KindFile Kind = "file"
)

// HomeRequest names the directory to validate as CFLOW_HOME.
type HomeRequest struct {
	Path string
}

// HomeFacts is the proven posture of a safe CFLOW_HOME. It is returned
// only after the owner, mode, type, symlink, filesystem, and advisory
// lock checks have all passed, except that a filesystem or lock probe
// failure returns the partial facts alongside the fault so diagnostics
// can render them.
type HomeFacts struct {
	CanonicalPath  string
	OwnerUID       int
	EffectiveUID   int
	Mode           fs.FileMode
	FileSystem     string
	AdvisoryLockOK bool
}

// PathRequest names a managed path to validate. Root is the optional
// canonical containment root (normally the validated CFLOW_HOME); when
// set, Path must resolve to Root or to a path strictly inside it.
type PathRequest struct {
	Path string
	Root string
	Kind Kind
}

// PathFacts is the proven posture of a safe managed path.
type PathFacts struct {
	CanonicalPath string
	InsideRoot    bool
	IsRoot        bool
	OwnerUID      int
	EffectiveUID  int
	Kind          Kind
	Mode          fs.FileMode
}

// insecureFault builds the fail-closed fault for path and permission
// problems. The text is stable and never embeds the path, its contents,
// or other user-specific data; callers already know which path they asked
// about.
func insecureFault(reason string) error {
	return model.NewFault(model.CodeInsecureCFLOWHomePermissions, reason)
}

// CheckHome validates CFLOW_HOME: canonical identity (no symlink in the
// path), a real directory, owner-only mode, effective-owner match, a
// known-local POSIX filesystem, and proven advisory-lock semantics
// (design 19.1, PRD 约束 113). CheckHome never creates or modifies the
// home, so doctor can run it read-only.
func CheckHome(req HomeRequest) (HomeFacts, error) {
	info, clean, err := checkFinal(req.Path)
	if err != nil {
		return HomeFacts{}, err
	}
	if !info.IsDir() {
		return HomeFacts{}, insecureFault("home path is not a directory")
	}
	if err := checkOwner(info); err != nil {
		return HomeFacts{}, err
	}
	if err := checkOwnerOnlyMode(info); err != nil {
		return HomeFacts{}, err
	}
	facts := HomeFacts{
		CanonicalPath: clean,
		OwnerUID:      ownerUID(info),
		EffectiveUID:  os.Geteuid(),
		Mode:          info.Mode(),
	}
	fsName, err := probeLocalFileSystem(clean)
	if err != nil {
		return facts, insecureFault("filesystem cannot prove local POSIX and lock semantics")
	}
	facts.FileSystem = fsName
	if err := probeAdvisoryLock(clean); err != nil {
		return facts, insecureFault("advisory lock semantics cannot be proven")
	}
	facts.AdvisoryLockOK = true
	return facts, nil
}

// CheckPath validates one managed path: canonical identity, parent
// safety, containment inside Root when given, file type, effective-owner
// match, and owner-only mode. Existing unsafe permissions are reported,
// never repaired.
func CheckPath(req PathRequest) (PathFacts, error) {
	info, clean, err := checkFinal(req.Path)
	if err != nil {
		return PathFacts{}, err
	}
	facts := PathFacts{
		CanonicalPath: clean,
		OwnerUID:      ownerUID(info),
		EffectiveUID:  os.Geteuid(),
		Mode:          info.Mode(),
	}
	switch {
	case info.IsDir():
		facts.Kind = KindDir
	case info.Mode().IsRegular():
		facts.Kind = KindFile
	default:
		return PathFacts{}, insecureFault("path is not a directory or regular file")
	}
	kind := req.Kind
	if kind == "" {
		kind = KindAny
	}
	if kind != KindAny && kind != facts.Kind {
		return PathFacts{}, insecureFault("path has an unexpected file type")
	}
	if err := checkOwner(info); err != nil {
		return PathFacts{}, err
	}
	if err := checkOwnerOnlyMode(info); err != nil {
		return PathFacts{}, err
	}
	if req.Root != "" {
		if err := requireValidRoot(req.Root); err != nil {
			return PathFacts{}, err
		}
		root := filepath.Clean(req.Root)
		switch {
		case clean == root:
			facts.InsideRoot, facts.IsRoot = true, true
		case strings.HasPrefix(clean, root+string(filepath.Separator)):
			facts.InsideRoot = true
		default:
			return PathFacts{}, insecureFault("path escapes the managed root")
		}
	}
	return facts, nil
}

// CreateSensitiveFile creates one sensitive file with O_CREATE|O_EXCL and
// mode 0600 inside a verified managed parent. An existing path is never
// reused; a just-created file whose born mode is not exactly 0600 is
// removed and fails closed rather than silently repaired. The caller
// writes and closes the returned file.
func CreateSensitiveFile(path string) (*os.File, error) {
	clean, err := checkParentForCreate(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, createError(clean, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, insecureFault("cannot verify the created file")
	}
	if err := checkOwner(info); err != nil {
		f.Close()
		os.Remove(clean)
		return nil, err
	}
	if info.Mode().Perm() != 0o600 {
		f.Close()
		os.Remove(clean)
		return nil, insecureFault("created file was not born with owner-only mode")
	}
	return f, nil
}

// CreateSensitiveDir creates one managed directory with mode 0700 (never
// MkdirAll: parents must exist as verified managed directories first).
// An existing path is never reused; a just-created directory whose born
// mode is not exactly 0700 is removed and fails closed.
func CreateSensitiveDir(path string) error {
	clean, err := checkParentForCreate(path)
	if err != nil {
		return err
	}
	if err := os.Mkdir(clean, 0o700); err != nil {
		return createError(clean, err)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return insecureFault("cannot verify the created directory")
	}
	if err := checkOwner(info); err != nil {
		os.Remove(clean)
		return err
	}
	if info.Mode().Perm() != 0o700 {
		os.Remove(clean)
		return insecureFault("created directory was not born with owner-only mode")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal path machinery
// ---------------------------------------------------------------------------

// checkFinal validates the canonical identity of path (absolute, cleaned,
// equal to its own EvalSymlinks form, every ancestor a real non-symlink
// directory with no group- or other-writable bits) and returns the final
// component's info and the cleaned path.
func checkFinal(path string) (os.FileInfo, string, error) {
	clean, err := canonicalize(path)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, "", insecureFault("path does not exist or cannot be inspected")
	}
	return info, clean, nil
}

// canonicalize rejects relative, empty, or symlink-bearing paths and
// returns the cleaned path, which by construction is already canonical.
func canonicalize(path string) (string, error) {
	if path == "" {
		return "", model.InvalidInputFault("path must be absolute")
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", model.InvalidInputFault("path must be absolute")
	}
	canon, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", insecureFault("path cannot be resolved safely")
	}
	if canon != clean {
		return "", insecureFault("path resolves through a symbolic link")
	}
	if err := checkAncestors(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// checkAncestors verifies every parent of clean: each is a real directory
// (never a symlink) and none is writable by group or other users, which
// would let a peer user swap the directory out from under the managed
// path (parent safety, design 19.1).
func checkAncestors(clean string) error {
	ancestor := filepath.Dir(clean)
	for {
		info, err := os.Lstat(ancestor)
		if err != nil {
			return insecureFault("parent directory cannot be inspected")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return insecureFault("path traverses a symbolic link")
		}
		if !info.IsDir() {
			return insecureFault("parent is not a directory")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return insecureFault("parent directory is writable by group or other users")
		}
		if ancestor == string(filepath.Separator) {
			return nil
		}
		ancestor = filepath.Dir(ancestor)
	}
}

// checkParentForCreate validates the parent of a planned creation and
// returns the cleaned target path.
func checkParentForCreate(path string) (string, error) {
	if path == "" {
		return "", model.InvalidInputFault("path must be absolute")
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", model.InvalidInputFault("path must be absolute")
	}
	parent := filepath.Dir(clean)
	if _, err := canonicalize(parent); err != nil {
		return "", err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", insecureFault("parent does not exist or cannot be inspected")
	}
	if err := checkOwner(info); err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", insecureFault("parent is not owner-only")
	}
	return clean, nil
}

// requireValidRoot validates the containment root with the same strict
// walk and requires it to be a directory.
func requireValidRoot(root string) error {
	info, _, err := checkFinal(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return insecureFault("containment root is not a directory")
	}
	return nil
}

// checkOwner fails closed when the path is not owned by the effective
// user: a peer account with write access to the file would then be able
// to read or replace it.
func checkOwner(info os.FileInfo) error {
	if ownerUID(info) != os.Geteuid() {
		return insecureFault("path is not owned by the effective user")
	}
	return nil
}

// checkOwnerOnlyMode fails closed when the path carries any group or
// other permission bits (PRD 约束 2). CFlow reports, never repairs.
func checkOwnerOnlyMode(info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return insecureFault("path has group or other permission bits")
	}
	return nil
}

// createError maps creation failures to stable faults. An existing path
// is a caller-protocol violation; everything else fails closed as a
// security posture problem. The OS error is never embedded in the fault
// text because it may carry the path.
func createError(path string, err error) error {
	if os.IsExist(err) {
		return model.InvalidInputFault("managed path already exists")
	}
	return insecureFault("cannot create managed path")
}
