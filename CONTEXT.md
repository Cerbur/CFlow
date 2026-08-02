# CFlow Domain Language

CFlow manages the local Plan-to-Done lifecycle of a coding requirement. This glossary fixes the domain terms used by the PRD, design, CLI, state model, artifacts, and future implementation.

## Scope and lifecycle

**Project**:
A Git repository known to CFlow, identified from its canonical repository path.
_Avoid_: Workspace, codebase, folder

**Workflow**:
The durable lifecycle of one user requirement from discussion through verified Integration output and final report.
_Avoid_: Job, pipeline, session

**Workflow Revision**:
An approved replacement of the active execution definition while preserving the Workflow's prior history.
_Avoid_: Workflow overwrite, reset

**Stage**:
The product phase currently occupied by a Workflow, such as Plan Check, Spec Generation, Execution, or Final Verification.
_Avoid_: Status, step

**Runtime Status**:
The operational condition of a Workflow within its Stage, such as Running, Paused, Blocked, Failed, Succeeded, or Cancelled.
_Avoid_: Stage, task status

**CFlow Runtime**:
The sole domain actor allowed to judge evidence and advance authoritative CFlow state.
_Avoid_: Agent, scheduler, provider

## Definitions and evidence

**Artifact Revision**:
An immutable, versioned CFlow document whose identity includes its type, revision, schema version, and content hash.
_Avoid_: Mutable config, current file

**Plan**:
The approved description of a requirement's goals, scope, constraints, repository analysis, solution direction, risks, and acceptance strategy.
_Avoid_: Task list, live status document

**Spec**:
The independently executable and reviewable definition of one unit of work, including dependencies, scopes, locks, routing, budget, and acceptance.
_Avoid_: Prompt, ticket text, node

**Dynamic Workflow**:
The restricted declarative DAG compiled from approved Specs and optional validated scheduling patches.
_Avoid_: Script, arbitrary workflow code, plan

**Verification Catalog**:
The immutable set of named deterministic commands that a Workflow is approved to invoke by reference.
_Avoid_: Shell script, command string, plugin

**Routing Policy**:
The immutable mapping from Agent Purpose to approved Provider, model, fallback, budget, and protocol binding.
_Avoid_: Provider config, permission sandbox

**Context Bundle**:
An immutable, redacted handoff package used when a Session cannot be resumed or work moves to another Provider.
_Avoid_: Recreated hidden context, raw transcript

**Evidence**:
A persisted fact used by the Runtime to judge an outcome, such as a Commit, test result, review result, protocol event, or Git snapshot.
_Avoid_: Agent claim, log message

**Finding**:
A structured, evidence-backed condition that prevents or constrains safe progress until the Runtime can resolve it from facts.
_Avoid_: Exception, warning, retry

## Execution

**Run**:
One coordinated foreground execution of a Workflow between start or resume and its next stable stop.
_Avoid_: Workflow, session, attempt

**Node**:
One schedulable operation in a Dynamic Workflow, such as Agent Task, Verify, Merge, Checkpoint, or Final Verify.
_Avoid_: Task, attempt, command

**Attempt**:
One immutable execution record for a Node, including its start facts, end facts, evidence, and retry charge.
_Avoid_: Retry, run, mutable node state

**Task**:
The user-facing projection of a Spec's execution, verification, and merge Nodes plus their Git evidence.
_Avoid_: Independent task state machine, node

**Session**:
One Provider-managed conversation identity used for exactly one Agent Purpose and role lineage.
_Avoid_: Run, workflow, terminal

**Provider**:
An installed coding-agent CLI that CFlow can drive through a supported structured protocol.
_Avoid_: Model, agent role, API service

**Agent Purpose**:
The constrained role assigned to a Session, such as Planning, Implementation, Review, Repair, or Final Verification.
_Avoid_: Provider, permission level

**Repair**:
A new, auditable execution path that addresses failed evidence without rewriting the failed Attempt or trusted history.
_Avoid_: Retry, reset, amend

**Retry Budget**:
The approved upper bound on automatic successor Attempts for a defined failure scope.
_Avoid_: Timeout, unlimited retry

**Checkpoint**:
A persisted, evidence-backed stable point from which the Runtime can later reconcile and continue safely.
_Avoid_: Approval, snapshot-only status

## Git delivery

**Planning Snapshot**:
The managed Git view fixed at the Workflow Base Commit for non-coding Agent Purposes.
_Avoid_: User working tree, integration worktree

**Task Worktree**:
The isolated managed Git worktree in which one coding Task produces its append-only Commit history.
_Avoid_: User working tree, shared workspace

**Integration Branch**:
The CFlow-owned branch that serially accumulates verified Task Commit histories for one Workflow.
_Avoid_: Target Branch, main branch

**Target Branch**:
The user branch recorded when a Workflow is created and modified only by a later explicit protected Apply.
_Avoid_: Integration Branch, current branch

**Apply**:
A post-completion, user-initiated delivery attempt that revalidates Integration output and may fast-forward the Target Branch.
_Avoid_: Merge Node, automatic delivery

**Quarantine**:
The permanent exclusion of a Branch or execution path from the trusted delivery chain while preserving its evidence.
_Avoid_: Deletion, rollback, ignored failure

**Cleanup**:
An explicit, separately confirmed lifecycle operation that may remove only fact-matching clean managed directories while preserving history and evidence.
_Avoid_: Cancel, garbage collection, reset

## Decisions and control

**Plan Approval**:
The append-only user decision accepting one exact checked Plan Revision and hash.
_Avoid_: Checker pass, general consent

**Execution Approval**:
The append-only user decision accepting one exact set of execution Artifacts, routing, budgets, and commit-policy facts.
_Avoid_: Plan Approval, blanket permission

**Blocking**:
The stable condition in which automatic progress is forbidden until new evidence, an approved revision, or an explicit user action resolves a Finding.
_Avoid_: Failed Workflow, paused

**Quiescing**:
The controlled condition that closes new dispatch while only the already recorded in-flight Attempts are allowed to settle.
_Avoid_: Paused, stopping, fail-fast

**Controlled Stop**:
The bounded two-phase process that stops dispatch, cancels managed processes, drains valid events, escalates termination, and persists interruption evidence.
_Avoid_: Immediate exit, cancel Workflow
