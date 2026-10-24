# rmapi-src — DESIGN (BACKFILL 2026-10-24)

Backfilled from README.md since code existed without a DESIGN.md.

## What this is

A maintained fork of **rMAPI** (`ddvk/rmapi`), a Go CLI/shell for the reMarkable Cloud API
(list/upload/download/backup documents, scriptable non-interactively). The upstream project was
archived by its original maintainer; this fork keeps it running rather than depending on a dead
repo.

## Why it's here, not under `3rdparty/`

This is vendored *source* under active local maintenance (not a frozen clone used as-is), so it
lives as a first-party-maintained project rather than `skills/3rdparty/`. Treat upstream-sourced
code here with the same caution as `3rdparty/` elsewhere: diverging silently from what the
original project did is a maintenance trap now that there's no upstream to re-sync against.

## How it's used

Drives Matt's reMarkable workflows (see `Tools Repo/Working/remarkable-*` investigations) —
print-to-reMarkable, backups, programmatic document upload.

## Status as of backfill

No open local-divergence tracking beyond this file; `script/prepare-release.sh` and
`annotations/test.sh` are the two local scripts layered on the Go source tree.
