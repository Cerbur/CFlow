#!/bin/sh
set -eu
# CFlow final verification wrapper (Purpose final_verify): full
# deterministic verification over the integration range.
go test ./...
