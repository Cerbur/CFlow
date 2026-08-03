package compile

// The typed IR of the Workflow Compiler (design 11, PRD 已确认：Dynamic
// Workflow 生成模型): the parsed Spec, Verification Catalog, restricted
// Patch, and the canonical Dynamic Workflow body. Every body the
// Compiler accepts is validated against the embedded JSON Schema first
// (artifact.ValidateBody, the same validation the Artifact Store applies
// on Put), so the typed views below are safe projections of already
// schema-valid documents.
//
// The Compiler's canonical Workflow serialization is the fixed struct
// field order plus canonically sorted nodes and edges; the body hash is
// the digest of exactly those bytes, so identical canonical inputs
// always produce the identical Artifact body and hash.

import (
	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
)

// nodeIDPattern is the closed form of node and Spec ids: the same form
// the workflow.json node id pattern binds, so every derived node id is
// schema-valid by construction.
const nodeIDPattern = `^[a-z0-9][a-z0-9_-]*$`

// Workflow node types (the workflow.json enum).
const (
	nodeTypeAgentTask   = "agent_task"
	nodeTypeVerify      = "verify"
	nodeTypeMerge       = "merge"
	nodeTypeCheckpoint  = "checkpoint"
	nodeTypeFinalVerify = "final_verify"
)

// Workflow Schema constants.
const (
	workflowSchema      = "cflow-workflow-1"
	workflowPatchSchema = "cflow-workflow-patch-1"
)

// ---------------------------------------------------------------------------
// typed IR
// ---------------------------------------------------------------------------

// Spec is one parsed Spec body (spec.json).
type Spec struct {
	ID             string     `yaml:"id"`
	Goal           string     `yaml:"goal"`
	DependsOn      []string   `yaml:"depends_on"`
	WriteScope     []string   `yaml:"write_scope"`
	ReadScope      []string   `yaml:"read_scope"`
	Locks          []string   `yaml:"locks"`
	Acceptance     Acceptance `yaml:"acceptance"`
	Route          *Route     `yaml:"route"`
	TimeoutSeconds int        `yaml:"timeout_seconds"`
	MaxRetry       int        `yaml:"max_retry"`
}

// Acceptance is a Spec's acceptance contract: the deterministic
// Verification Command ids plus whether an independent semantic review
// is required.
type Acceptance struct {
	VerificationCommandIDs []string `yaml:"verification_command_ids"`
	ReviewRequired         bool     `yaml:"review_required"`
}

// Route is one Spec's approved Agent Route.
type Route struct {
	Provider string  `yaml:"provider"`
	Model    string  `yaml:"model"`
	Budget   float64 `yaml:"budget"`
}

// CatalogEntry is one parsed Verification Catalog entry (catalog.json).
type CatalogEntry struct {
	CommandID           string   `yaml:"command_id"`
	Executable          string   `yaml:"executable"`
	Args                []string `yaml:"args"`
	CWD                 string   `yaml:"cwd"`
	Purpose             string   `yaml:"purpose"`
	TimeoutSeconds      int      `yaml:"timeout_seconds"`
	ExpectedExitCodes   []int    `yaml:"expected_exit_codes"`
	MaxOutputBytes      int      `yaml:"max_output_bytes"`
	Env                 []string `yaml:"env"`
	TransientWritePaths []string `yaml:"transient_write_paths"`
	Source              string   `yaml:"source"`
}

// Catalog is one parsed Verification Catalog body (catalog.json).
type Catalog struct {
	Revision int            `yaml:"revision"`
	Entries  []CatalogEntry `yaml:"entries"`
}

// PatchOp is one restricted scheduling operation (workflow-patch.json).
type PatchOp struct {
	Op          string  `yaml:"op"`
	NodeID      string  `yaml:"node_id"`
	MaxParallel int     `yaml:"max_parallel"`
	Provider    string  `yaml:"provider"`
	Budget      float64 `yaml:"budget"`
}

// Patch is one parsed Patch IR body (workflow-patch.json).
type Patch struct {
	Schema     string    `yaml:"schema"`
	Operations []PatchOp `yaml:"operations"`
}

// WorkflowNode is one node of the compiled Dynamic Workflow.
type WorkflowNode struct {
	ID             string `yaml:"id"`
	Type           string `yaml:"type"`
	SpecID         string `yaml:"spec_id,omitempty"`
	CommandID      string `yaml:"command_id,omitempty"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"`
	MaxRetry       int    `yaml:"max_retry,omitempty"`
}

// WorkflowEdge is one directed edge of the compiled Dynamic Workflow.
type WorkflowEdge struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Workflow is the canonical Dynamic Workflow body (workflow.json).
type Workflow struct {
	Schema     string         `yaml:"schema"`
	WorkflowID string         `yaml:"workflow_id"`
	Revision   int            `yaml:"revision"`
	Nodes      []WorkflowNode `yaml:"nodes"`
	Edges      []WorkflowEdge `yaml:"edges"`
}

// ---------------------------------------------------------------------------
// parsing (schema validation first)
// ---------------------------------------------------------------------------

// ParseSpec parses and validates one Spec body against the embedded
// spec.json schema. Free argv, unknown fields, and schema violations
// fail with SCHEMA_INVALID (brief TestSpecRejectsFreeArgv).
func ParseSpec(body []byte) (Spec, error) {
	if err := artifact.ValidateBody("spec.json", body); err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := yaml.Unmarshal(body, &s); err != nil {
		return Spec{}, schemaInvalid("spec body cannot be parsed")
	}
	return s, nil
}

// ParseWorkflow parses and validates one Dynamic Workflow body against
// the embedded workflow.json schema.
func ParseWorkflow(body []byte) (Workflow, error) {
	if err := artifact.ValidateBody("workflow.json", body); err != nil {
		return Workflow{}, err
	}
	var w Workflow
	if err := yaml.Unmarshal(body, &w); err != nil {
		return Workflow{}, schemaInvalid("workflow body cannot be parsed")
	}
	return w, nil
}

// ParseCatalog parses and validates one Verification Catalog body
// against the embedded catalog.json schema.
func ParseCatalog(body []byte) (Catalog, error) {
	return parseCatalog(body)
}

// parseCatalog parses and validates one Catalog body against the
// embedded catalog.json schema.
func parseCatalog(body []byte) (Catalog, error) {
	if err := artifact.ValidateBody("catalog.json", body); err != nil {
		return Catalog{}, err
	}
	var c Catalog
	if err := yaml.Unmarshal(body, &c); err != nil {
		return Catalog{}, schemaInvalid("catalog body cannot be parsed")
	}
	return c, nil
}

// parsePatch parses and validates one Patch IR body against the embedded
// workflow-patch.json schema. The Patch is the restricted scheduling IR:
// ANY schema violation — an unknown operation, a missing schema marker,
// an extra field — is a forbidden attempt to express an operation
// outside the four allowed Patch operations, and fails with
// WORKFLOW_PATCH_FORBIDDEN (brief TestPatchCannotRemoveVerifyNode).
func parsePatch(body []byte) (Patch, error) {
	if err := artifact.ValidateBody("workflow-patch.json", body); err != nil {
		return Patch{}, model.NewFault(model.CodeWorkflowPatchForbidden,
			"patch body fails the restricted patch schema")
	}
	var p Patch
	if err := yaml.Unmarshal(body, &p); err != nil {
		return Patch{}, model.NewFault(model.CodeWorkflowPatchForbidden,
			"patch body cannot be parsed")
	}
	return p, nil
}

func schemaInvalid(text string) error {
	return model.NewFault(model.CodeSchemaInvalid, text)
}
