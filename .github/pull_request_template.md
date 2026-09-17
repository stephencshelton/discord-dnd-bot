<!--
AGENTS.md §6 (git conduct) and §7 (evidence, not assertion) are what this template enforces.
Delete a section only if it genuinely does not apply — an empty Risk or Verification section
is itself a review finding.
-->

## What changed

<!-- The change in a few sentences. What a reviewer should expect to see in the diff. -->

## Why

<!-- The problem, not the solution restated. If it fixes a reported failure, quote the log line
     or error and say what it actually meant. -->

## Risk

<!-- Does this touch /session start, /session stop, the voice manager, the checkpoint loop, or the
     transcribe job? This bot records real sessions on scheduled evenings (AGENTS.md §1), so either
     state plainly what could go wrong at a live table, or say why that path cannot be affected —
     and say how you checked, rather than asserting it.

     Also call out: anything that changes ordering or control flow, anything a running deployment
     picks up on restart, and anything that is not reversible by rolling back the image. -->

## Verification

<!-- Evidence, not assertion (AGENTS.md §7). Tick only what you actually ran. -->

- [ ] `gofmt -l .` — clean
- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test -race ./...`
- [ ] `golangci-lint run ./...` — 0 issues
- [ ] `helm lint charts/discord-dnd-bot` (only if the chart changed)

<!-- Paste failures rather than omitting them, and name anything you could NOT verify. There is no
     harness that exercises a Discord interaction end to end and the production token must never be
     used locally, so "verified by unit tests and reasoning" is an honest answer — an unstated gap
     is not. -->

## Configuration

- [ ] No new or changed knob.
- [ ] New/changed knob, plumbed through **all** of: `internal/config/config.go` (field, default,
      comment saying what breaks at the wrong value, resolver), `charts/discord-dnd-bot/values.yaml`,
      `charts/discord-dnd-bot/templates/configmap.yaml`, and the README configuration table.
- [ ] A constant derived from an existing knob — documented at both ends, including in values.yaml.

## Database

- [ ] No schema change.
- [ ] New numbered migration in `internal/db/migrations/`, idempotent (it re-runs on every boot),
      with no already-shipped migration edited.

## Known, not addressed here

<!-- Anything found along the way and deliberately left out, so it is on the record rather than
     rediscovered later. Scaling the work down is the operator's call — name it here. -->
