// Package artifact implements the immutable Artifact Store (design 10):
// the embedded Compatibility Registry, canonical hashing, and the atomic
// owner-only write protocol that persists one immutable Artifact Revision
// per (workflow_id, artifact_type, revision, sha256) identity (design
// 7.2).
//
// Write protocol (design 10.2): validate the body against its embedded
// schema, redact all content (Task 3 Redactor), canonically serialize and
// hash, reject an existing target path even when the content appears
// equal, write a same-directory 0600 temporary file that never follows
// symlinks, write/flush/fsync/close/rename atomically (no-replace), fsync
// the parent directory, reopen through the Artifact Reader and verify
// type, version, hash, owner, mode, and path containment, then return the
// immutable reference for the Store transaction. The reader verifies
// owner, mode, canonical path, size, schema compatibility, and content
// hash on every Get.
//
// Concurrency: the Application serializes artifact writes per Project
// under the Writer Lease and Workflow Owner (design 18); within that
// discipline the atomic no-replace rename makes an existing-path write
// race fail instead of overwriting approved content.
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// compatibilityRegistry maps every Artifact Type to the explicit Reader
// versions this binary supports (design 10.3, PRD 约束 10-11). Unsupported
// versions fail with ARTIFACT_SCHEMA_UNSUPPORTED before body
// deserialization. A newer binary may create a new derived Revision
// through the normal gates but never edits an old Artifact in place.
var compatibilityRegistry = map[model.ArtifactType][]string{
	model.ArtifactPlan:            {"1.0.0"},
	model.ArtifactSpec:            {"1.0.0"},
	model.ArtifactWorkflow:        {"1.0.0"},
	model.ArtifactCatalog:         {"1.0.0"},
	model.ArtifactReport:          {"1.0.0"},
	model.ArtifactCleanupManifest: {"1.0.0"},
	// Task 10 planning artifacts: requirement turns and Plan Check
	// results carry no agent body schema (the Kernel validates the
	// structured check result before the write is requested).
	model.ArtifactDiscussionTurn: {"1.0.0"},
	model.ArtifactPlanCheck:      {"1.0.0"},
}

// supportedVersion reports whether a declared Artifact Type's schema
// version has an explicit Reader version in this binary.
func supportedVersion(typ model.ArtifactType, version string) bool {
	for _, v := range compatibilityRegistry[typ] {
		if v == version {
			return true
		}
	}
	return false
}

// errInjected is the sentinel every injected fault point returns.
var errInjected = errors.New("artifact: injected failure")

// FaultPoint names one crash/injection point inside the Store. The points
// are test seams set only inside this package (design 22.1 atomic-write
// fault injector) and are never reachable from release code paths.
type FaultPoint string

const (
	// FailBeforeRename aborts a Put after the temporary file is written
	// and fsynced, before the atomic rename: no target may exist.
	FailBeforeRename FaultPoint = "fail-before-rename"
	// FailAfterRename aborts a Put after the rename, before parent
	// fsync and verification: the unverified target must be removed.
	FailAfterRename FaultPoint = "fail-after-rename"
)

// Store is the immutable Artifact Store over one managed artifacts root.
type Store struct {
	root      string
	redaction security.Registry
	inject    map[FaultPoint]struct{} // test-only injection points
}

// PutRequest names everything one Artifact write binds: the immutable
// identity fields, the envelope metadata, and the authored body. The body
// is validated, redacted, and canonically serialized before anything is
// written.
type PutRequest struct {
	WorkflowID    model.WorkflowID
	Type          model.ArtifactType
	Revision      int
	SchemaVersion string
	CreatedAt     string
	Producer      ProducerRef
	InputRefs     []InputRef
	Body          []byte
}

// ResolveRequest names an Artifact by (workflow, type, revision); a zero
// Revision resolves the latest revision present on disk.
type ResolveRequest struct {
	WorkflowID model.WorkflowID
	Type       model.ArtifactType
	Revision   int
}

// New constructs a Store over root, the canonical absolute artifacts root
// (normally CFLOW_HOME/artifacts, validated by the caller through the
// Security Guard). The redaction registry is the embedded CFlow-owned
// rule set the Store applies to every body before serialization.
func New(root string, redaction security.Registry) (*Store, error) {
	if root == "" {
		return nil, model.InvalidInputFault("artifacts root must be an absolute path")
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return nil, model.InvalidInputFault("artifacts root must be an absolute path")
	}
	if _, err := os.Lstat(clean); err == nil {
		if _, err := security.CheckPath(security.PathRequest{Path: clean, Kind: security.KindDir}); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, storeFault("artifacts root cannot be inspected")
	}
	for _, rule := range redaction.Rules {
		if err := validateRule(rule); err != nil {
			return nil, err
		}
	}
	return &Store{root: clean, redaction: redaction}, nil
}

// Put validates, redacts, and canonically serializes the body, writes one
// immutable Artifact Revision through the atomic owner-only write
// protocol, verifies the stored file through the Artifact Reader, and
// returns the immutable reference for the Store transaction.
func (s *Store) Put(ctx context.Context, req PutRequest) (model.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return model.ArtifactRef{}, err
	}
	if err := validatePutRequest(req); err != nil {
		return model.ArtifactRef{}, err
	}
	if !supportedVersion(req.Type, req.SchemaVersion) {
		return model.ArtifactRef{}, model.NewFault(model.CodeArtifactSchemaUnsupported,
			"artifact schema version is not supported by this binary")
	}
	if err := validateBody(req.Type, req.Body); err != nil {
		return model.ArtifactRef{}, err
	}
	redacted, facts, err := redactBody(s.redaction, req.Body)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	canonical, err := Canonicalize(model.ArtifactEnvelope{
		Type:          req.Type,
		Revision:      req.Revision,
		SchemaVersion: req.SchemaVersion,
		Payload:       redacted,
	})
	if err != nil {
		return model.ArtifactRef{}, err
	}
	// The canonical serialization carries the canonical content the file
	// stores; the reader rebuilds the same wrapper from the stored fields
	// to verify the digest.
	var ce fileEnvelope
	if err := json.Unmarshal(canonical, &ce); err != nil {
		return model.ArtifactRef{}, storeFault("canonical serialization cannot be parsed")
	}
	ref := model.ArtifactRef{
		Workflow: req.WorkflowID,
		Type:     req.Type,
		Revision: req.Revision,
		Hash:     HashCanonical(canonical),
	}
	if err := s.writeArtifact(ctx, req, ce.Content, facts, ref); err != nil {
		return model.ArtifactRef{}, err
	}
	return ref, nil
}

// Get returns the canonical content of one immutable Artifact Revision
// after verifying owner, mode, canonical path, size, schema
// compatibility, and content hash (design 10.2.7).
func (s *Store) Get(ctx context.Context, ref model.ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, string(ref.Workflow), string(ref.Type),
		strconv.Itoa(ref.Revision), ref.Hash)
	return s.readAndVerify(ctx, path, ref)
}

// Resolve binds (workflow, type, revision) to the stored content hash,
// fully verifying the stored file. A zero Revision resolves the latest
// revision present on disk.
func (s *Store) Resolve(ctx context.Context, req ResolveRequest) (model.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return model.ArtifactRef{}, err
	}
	if invalidWorkflowID(req.WorkflowID) {
		return model.ArtifactRef{}, model.InvalidInputFault("workflow id is not a safe managed path component")
	}
	if !req.Type.Valid() {
		return model.ArtifactRef{}, model.InvalidInputFault("unknown artifact type")
	}
	if req.Revision < 0 {
		return model.ArtifactRef{}, model.InvalidInputFault("artifact revision must not be negative")
	}
	base := filepath.Join(s.root, string(req.WorkflowID), string(req.Type))
	revision := req.Revision
	if revision == 0 {
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				return model.ArtifactRef{}, model.InvalidInputFault("artifact revision not found")
			}
			return model.ArtifactRef{}, storeFault("artifact revision directory cannot be read")
		}
		max := 0
		for _, e := range entries {
			if !e.IsDir() {
				return model.ArtifactRef{}, storeFault("artifact layout holds an unexpected file")
			}
			n, err := strconv.Atoi(e.Name())
			if err != nil {
				return model.ArtifactRef{}, storeFault("artifact layout holds an unexpected directory")
			}
			if n > max {
				max = n
			}
		}
		if max == 0 {
			return model.ArtifactRef{}, model.InvalidInputFault("artifact revision not found")
		}
		revision = max
	}
	dir := filepath.Join(base, strconv.Itoa(revision))
	hash, err := revisionContentHash(dir)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	ref := model.ArtifactRef{Workflow: req.WorkflowID, Type: req.Type, Revision: revision, Hash: hash}
	if _, err := s.readAndVerify(ctx, filepath.Join(dir, hash), ref); err != nil {
		return model.ArtifactRef{}, err
	}
	return ref, nil
}

// ---------------------------------------------------------------------------
// Write protocol machinery (design 10.2)
// ---------------------------------------------------------------------------

// writeArtifact performs steps 4-8 of the write protocol: reject the
// existing target and any conflicting revision content, create the managed
// tree, write a same-directory 0600 temporary file, fsync, close, rename
// atomically without replacement, fsync the parent directory, and verify
// the stored file through the Artifact Reader.
func (s *Store) writeArtifact(ctx context.Context, req PutRequest, content []byte, facts redactionFacts, ref model.ArtifactRef) error {
	dir := filepath.Join(s.root, string(req.WorkflowID), string(req.Type), strconv.Itoa(req.Revision))
	target := filepath.Join(dir, ref.Hash)

	// Reject an existing target path even when the content appears equal;
	// idempotency is resolved through the recorded intent (design 10.2.4).
	if _, err := os.Lstat(target); err == nil {
		return model.InvalidInputFault("artifact target path already exists; idempotency must be resolved through recorded intent")
	} else if !os.IsNotExist(err) {
		return storeFault("artifact target path cannot be inspected")
	}
	// A revision directory holds exactly one content file; a second write
	// with different content is a revision conflict, never an overwrite.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if isContentFile(e.Name()) {
				return model.InvalidInputFault("artifact revision is already assigned to different content")
			}
		}
	} else if !os.IsNotExist(err) {
		return storeFault("artifact revision directory cannot be inspected")
	}

	for _, d := range []string{
		s.root,
		filepath.Join(s.root, string(req.WorkflowID)),
		filepath.Join(s.root, string(req.WorkflowID), string(req.Type)),
		dir,
	} {
		if err := s.ensureDir(d); err != nil {
			return err
		}
	}

	fe := fileEnvelope{
		SchemaVersion: req.SchemaVersion,
		ArtifactType:  req.Type,
		WorkflowID:    req.WorkflowID,
		Revision:      req.Revision,
		CreatedAt:     req.CreatedAt,
		InputRefs:     req.InputRefs,
		Redaction:     &facts,
		ContentSHA256: ref.Hash,
		Content:       content,
	}
	if req.Producer != (ProducerRef{}) {
		p := req.Producer
		fe.Producer = &p
	}
	data, err := marshalCanonical(fe)
	if err != nil {
		return err
	}
	if len(data) > maxFileBytes {
		return model.InvalidInputFault("artifact exceeds the bounded file size")
	}

	temp := filepath.Join(dir, "."+ref.Hash+".tmp."+randomHex())
	f, err := security.CreateSensitiveFile(temp)
	if err != nil {
		return err
	}
	if err := writeAll(f, data); err != nil {
		f.Close()
		os.Remove(temp)
		return storeFault("artifact file cannot be written")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(temp)
		return storeFault("artifact file cannot be flushed")
	}
	if err := f.Close(); err != nil {
		os.Remove(temp)
		return storeFault("artifact file cannot be closed")
	}
	if err := ctx.Err(); err != nil {
		os.Remove(temp)
		return err
	}
	if err := s.injectFault(FailBeforeRename); err != nil {
		os.Remove(temp)
		return err
	}
	if err := renameNoReplace(temp, target); err != nil {
		os.Remove(temp)
		if errors.Is(err, os.ErrExist) {
			return model.InvalidInputFault("artifact target path already exists")
		}
		return storeFault("artifact cannot be atomically installed")
	}
	if err := ctx.Err(); err != nil {
		os.Remove(target)
		return err
	}
	if err := s.injectFault(FailAfterRename); err != nil {
		os.Remove(target)
		return err
	}
	if err := fsyncDir(dir); err != nil {
		os.Remove(target)
		return storeFault("artifact directory cannot be flushed")
	}
	if err := s.verifyStored(ctx, target, ref); err != nil {
		os.Remove(target)
		return err
	}
	return nil
}

// verifyStored reopens the just-written file through the Artifact Reader
// and verifies type, version, hash, owner, mode, and path containment
// (design 10.2.7).
func (s *Store) verifyStored(ctx context.Context, path string, ref model.ArtifactRef) error {
	_, err := s.readAndVerify(ctx, path, ref)
	return err
}

// readAndVerify reads one stored artifact after proving the path posture,
// the canonical file form, the schema compatibility, the envelope/reference
// consistency, and the content hash.
func (s *Store) readAndVerify(ctx context.Context, path string, ref model.ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, model.InvalidInputFault("artifact not found")
		}
		return nil, storeFault("artifact path cannot be inspected")
	}
	if _, err := security.CheckPath(security.PathRequest{Path: path, Root: s.root, Kind: security.KindFile}); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, storeFault("artifact cannot be read")
	}
	if len(data) > maxFileBytes {
		return nil, storeFault("artifact exceeds the bounded file size")
	}
	var fe fileEnvelope
	if err := json.Unmarshal(data, &fe); err != nil {
		return nil, storeFault("artifact is not canonical CFlow content")
	}
	// The stored bytes must be exactly the canonical serialization of the
	// envelope: any trailing, reordered, duplicated, or foreign content is
	// rejected (this also verifies the size byte-for-byte).
	re, err := marshalCanonical(fe)
	if err != nil || !stringEqual(re, data) {
		return nil, storeFault("artifact is not canonical CFlow content")
	}
	// Schema compatibility is verified before the body is deserialized
	// (design 10.3).
	if !fe.ArtifactType.Valid() || !supportedVersion(fe.ArtifactType, fe.SchemaVersion) {
		return nil, model.NewFault(model.CodeArtifactSchemaUnsupported,
			"artifact schema is not supported by this binary")
	}
	if fe.WorkflowID != ref.Workflow || fe.ArtifactType != ref.Type || fe.Revision != ref.Revision {
		return nil, storeFault("artifact envelope does not match the requested reference")
	}
	// The digest is the hash of the canonical wrapper rebuilt from the
	// stored fields; the stored content is already canonical, so it is
	// never re-canonicalized here.
	if digest := canonicalDigest(fe); digest != ref.Hash || digest != fe.ContentSHA256 {
		return nil, storeFault("artifact content hash does not match its identity")
	}
	return fe.Content, nil
}

// canonicalDigest rebuilds the canonical wrapper of a stored envelope and
// hashes it. A marshal failure (a build bug, since the file already passed
// the canonical-form check) yields "" and fails the hash check closed.
func canonicalDigest(fe fileEnvelope) string {
	canonical, err := marshalCanonical(fileEnvelope{
		SchemaVersion: fe.SchemaVersion,
		ArtifactType:  fe.ArtifactType,
		Revision:      fe.Revision,
		Content:       fe.Content,
	})
	if err != nil {
		return ""
	}
	return HashCanonical(canonical)
}

// ensureDir verifies an existing managed directory or creates it 0700
// through the Security Guard; an existing path is never reused.
func (s *Store) ensureDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindDir}); err != nil {
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		return storeFault("managed directory cannot be inspected")
	}
	if err := security.CreateSensitiveDir(path); err != nil {
		return err
	}
	return nil
}

// injectFault reports whether a test-injected fault point fires.
func (s *Store) injectFault(p FaultPoint) error {
	if s.inject != nil {
		if _, ok := s.inject[p]; ok {
			return errInjected
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Small OS helpers
// ---------------------------------------------------------------------------

// writeAll writes every byte, retrying short writes.
func writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// fsyncDir flushes the directory entry of the atomic rename to disk.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// randomHex returns 12 hex characters of cryptographically random entropy
// for the temporary file name.
func randomHex() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("artifact: entropy source failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// storeFault is the fail-closed fault for data-safety failures under the
// managed artifacts tree: a file that cannot be written, flushed,
// installed, or verified is a posture problem, never silently repaired.
func storeFault(reason string) error {
	return model.NewFault(model.CodeInsecureCFLOWHomePermissions, reason)
}

// stringEqual avoids an allocation for the canonical-form byte check.
func stringEqual(a, b []byte) bool {
	return string(a) == string(b)
}
