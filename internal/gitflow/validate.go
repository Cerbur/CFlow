package gitflow

import (
	"os"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
)

// canonicalDir validates that dir is absolute, resolves to itself through
// realpath semantics, and is a real directory. It returns the canonical
// form.
func canonicalDir(dir string) (string, error) {
	if dir == "" {
		return "", model.InvalidInputFault("gitflow: working directory is empty")
	}
	if !filepath.IsAbs(dir) {
		return "", model.InvalidInputFault("gitflow: working directory must be absolute")
	}
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", model.InvalidInputFault("gitflow: working directory cannot be resolved")
	}
	info, err := os.Stat(canon)
	if err != nil || !info.IsDir() {
		return "", model.InvalidInputFault("gitflow: working directory is not a directory")
	}
	return canon, nil
}

// isFullHex reports whether s is a full object hash (SHA-1 or SHA-256).
func isFullHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// validateHead rejects anything that is not a full commit hash. Expected
// HEADs are always full hashes, never abbreviations or symbolic names:
// abbreviation resolution would silently observe a different commit than
// the one the caller recorded.
func validateHead(head string) error {
	if !isFullHex(head) {
		return model.InvalidInputFault("gitflow: expected head must be a full commit hash")
	}
	return nil
}

// validateRefName enforces the Git refname rules (git check-ref-format)
// plus a hard ban on leading dashes so a ref field can never be mistaken
// for an option.
func validateRefName(ref string) error {
	if ref == "" {
		return model.InvalidInputFault("gitflow: ref is empty")
	}
	if strings.HasPrefix(ref, "-") {
		return model.InvalidInputFault("gitflow: ref must not start with a dash")
	}
	if ref == "HEAD" || strings.HasPrefix(ref, "HEAD/") {
		return model.InvalidInputFault("gitflow: ref must not be HEAD")
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return model.InvalidInputFault("gitflow: ref has an empty component")
	}
	if len(ref) > 1024 {
		return model.InvalidInputFault("gitflow: ref is too long")
	}
	if strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return model.InvalidInputFault("gitflow: ref has an invalid suffix")
	}
	for _, bad := range []string{"..", "@{", "~", "^", ":", "?", "*", "[", "\\", " ", "\t", "\n", "\x00"} {
		if strings.Contains(ref, bad) {
			return model.InvalidInputFault("gitflow: ref contains an illegal character")
		}
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return model.InvalidInputFault("gitflow: ref contains a control character")
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || part == "." || part == ".." {
			return model.InvalidInputFault("gitflow: ref has an invalid component")
		}
		if len(part) > 255 {
			return model.InvalidInputFault("gitflow: ref component is too long")
		}
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") {
			return model.InvalidInputFault("gitflow: ref component has an invalid dot placement")
		}
	}
	return nil
}

// validateBranchName validates a branch name (a refname without the
// refs/heads/ prefix).
func validateBranchName(branch string) error {
	if branch == "" {
		return model.InvalidInputFault("gitflow: branch is empty")
	}
	if err := validateRefName(branch); err != nil {
		return err
	}
	if strings.HasPrefix(branch, "refs/") {
		return model.InvalidInputFault("gitflow: branch must not carry the refs/ prefix")
	}
	return nil
}

// validateAuditRef validates a full audit refname: it must live under the
// refs/ namespace.
func validateAuditRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/") {
		return model.InvalidInputFault("gitflow: audit ref must start with refs/")
	}
	return validateRefName(ref)
}
