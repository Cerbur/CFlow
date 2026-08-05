#!/bin/sh
set -eu
# CFlow integration verification wrapper (Purpose integration_verify):
# deterministic verification over the merged integration range.
go test ./...
