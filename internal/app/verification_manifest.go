package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"cflow.local/cflow/internal/model"
)

const verificationManifestSchemaVersion = "1.0.0"

// validateVerificationManifest is the single read-side integrity boundary for
// deterministic verification evidence. Review, Apply, and report consumers
// all require the exact schema and Node identity and recompute both content
// hashes before trusting any manifest field.
func validateVerificationManifest(body []byte, node model.NodeID) (model.EvidenceManifest, error) {
	var manifest model.EvidenceManifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s cannot be parsed: %w", node, err))
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s is not one canonical document: %w", node, err))
	}
	if manifest.SchemaVersion != verificationManifestSchemaVersion {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s has unsupported schema %q", node, manifest.SchemaVersion))
	}
	if manifest.Node != node {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s does not match its required node identity", node))
	}
	if manifest.OutputHash != verificationEvidenceHash([]byte(manifest.Output)) {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s has an invalid output hash", node))
	}
	actualHash := manifest.Hash
	manifest.Hash = ""
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s cannot be canonicalized: %w", node, err))
	}
	if actualHash == "" || actualHash != verificationEvidenceHash(canonical) {
		return model.EvidenceManifest{}, model.InvariantFault(fmt.Errorf("verification manifest for %s has an invalid self-hash", node))
	}
	manifest.Hash = actualHash
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func verificationEvidenceHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
