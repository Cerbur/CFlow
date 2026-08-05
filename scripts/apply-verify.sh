#!/bin/sh
set -eu
# CFlow apply verification wrapper (Purpose apply_verify): deterministic
# verification of the staged Apply result before the Target fast-forward.
go test ./...
