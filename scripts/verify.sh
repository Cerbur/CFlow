#!/bin/sh
set -eu
# CFlow task verification wrapper (discovered at the Base Commit by the
# Verification Catalog as command id "verify", Purpose task_verify).
# Bounded to the dogfood docs/tests-only requirement: the repository must
# build and the targeted test package must pass. CFlow separately
# enforces the Git-clean and write-scope gates; this wrapper only proves
# the deterministic build/test facts.
go build ./...
go test ./internal/observe
