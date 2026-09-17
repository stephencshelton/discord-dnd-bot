# Agent operating contract

This file is for any AI coding agent working in this repository — Claude Code, Copilot, an SDK agent,
or a human who wants the same discipline. It is the entry point. The [README](README.md) is the
operator's manual (configuration, deployment, model routing); this file is how to *change* the code
without breaking a live table.

## 1. What this repository is

A Discord bot that records a tabletop RPG session over voice, transcribes it, writes the session
notes, and answers questions about the campaign afterwards. Go 1.26, two binaries from one module:

- **`cmd/gateway`** — holds the Discord gateway connection, serves slash commands, captures voice,
  and enqueues slow work. It must stay responsive at all times.
- **`cmd/worker`** — a stateless Redis queue consumer that does the slow work: transcription,
  summarization, campaign-state extraction, embedding, and art generation.

Backing services: PostgreSQL with pgvector (campaign data and retrieval embeddings), Redis (job
queue), S3-compatible object storage (raw audio chunks), and a LiteLLM proxy that fronts every model
behind one OpenAI-compatible API. Deployment is Helm + ArgoCD; the gateway uses a `Recreate` strategy
because two live gateway connections on one token is a fault, not a scale-up.

**This bot is used at a real table on real evenings.** A change that lands broken does not fail a
test suite, it fails a group of people mid-session. Treat `/session start` and `/session stop` as the
highest-risk surface in the repo.

## 2. Build and test surface

There is no `make`/`just` wrapper — the Go tool is the command surface. CGO is required: the voice
path decodes Opus via `layeh.com/gopus`.

```bash
sudo apt-get install -y libopus-dev pkg-config   # once
export CGO_ENABLED=1

go build ./...
go vet ./...
go test -race ./...          # what CI runs
gofmt -l . && golangci-lint run ./...
```

Run a service locally with its environment configured:

```bash
CGO_ENABLED=1 go run ./cmd/gateway
CGO_ENABLED=1 go run ./cmd/worker
```

Before reporting any change complete: `gofmt -l .`, `go build ./...`, `go vet ./...`,
`go test -race ./...`, and `golangci-lint run ./...` — all green. `golangci-lint` is pinned to
**v2.13.1** in CI; older releases refuse to lint a `go 1.26.0` module. Its config
([.golangci.yml](.golangci.yml)) enables `errcheck`, `staticcheck`, `revive`, `bodyclose`,
`ineffassign`, `misspell`, `unconvert`, and `goimports` with the local prefix
`github.com/stephencshelton/discord-dnd-bot`.

CI runs three workflows and no model inference of any kind:

| Workflow | What it does |
| --- | --- |
| [ci.yml](.github/workflows/ci.yml) | golangci-lint; `go vet`; race tests with coverage to Codecov; `govulncheck`; `gosec` to SARIF |
| [docker.yml](.github/workflows/docker.yml) | builds the gateway and worker images, Trivy-scans them, pushes to GHCR |
| [helm.yml](.github/workflows/helm.yml) | lints and renders the chart, validates manifests with Kubeconform |

Actions are pinned to full commit SHAs with a version comment and jobs declare minimal
`permissions:`. Keep it that way when you touch a workflow. **CI failures are fixed, never bypassed**
— no `continue-on-error` on a step that failed, no skipping a check so a change can land.

## 3. The rules that are not negotiable

1. **Never miss Discord's response windows.** A handler has **3 seconds** to make an initial
   response. Anything slower must defer first, which buys **~15 minutes** to follow up. Fast handlers
   use `ictx.reply`; slow ones use `ictx.ackLong(ctx, ephemeral, budget)`, which defers *and* returns
   a context with a deadline that matches the dependency being waited on. The bare
   `interactionTimeout` (15s) exists only to protect the initial-response window — it is not a budget
   for a model call, a voice handshake, or an object-storage purge. Getting this wrong is what made
   `/prep` fail with `context deadline exceeded` at exactly 15.2s.
2. **The gateway never blocks on slow AI work it could hand off.** Transcription, art, embedding, and
   state extraction are queue jobs. If a new feature needs minutes, it is a job, not a handler.
3. **Job handlers are idempotent.** The queue is a reliable Redis list (`BRPOPLPUSH` onto a
   processing list, then `Ack`/`Requeue`), so delivery is **at-least-once** and a job can run twice.
   Wrap genuinely unretryable failures with `queue.Permanent(err)`; everything else is retried with
   an attempt counter.
4. **Migrations are append-only and idempotent.** [internal/db/migrations/](internal/db/migrations/)
   is embedded and applied in lexical order under a Postgres advisory lock, with **no
   applied-version tracking** — every file re-runs on every boot. Write `CREATE TABLE IF NOT
   EXISTS` / `ADD COLUMN IF NOT EXISTS`. Never edit a migration that has shipped; add the next
   number.
5. **No bare numeric literals, and the reasoning is always written down.** Which form a value takes
   depends on one question: *would an operator have a reason to change it for their deployment?*
   - **Yes** → an environment knob. Add an `envconfig` field in
     [internal/config/config.go](internal/config/config.go) with a default, a comment saying what
     breaks at the wrong value, and a resolver method that falls back when the value is unset or
     non-positive. Then plumb it through all three of
     [values.yaml](charts/discord-dnd-bot/values.yaml),
     [the configmap](charts/discord-dnd-bot/templates/configmap.yaml), and the README's configuration
     table — a knob missing from any one of them is a knob nobody can reach. Model routes, token
     budgets, timeouts against external services, and pool sizes are all knobs.
   - **No** → a named constant near its use, with a comment giving the number's derivation. A bound
     fixed by an upstream protocol is not an operational choice: Discord's 3-second response window
     and 15-minute followup window are the same on every deployment, so the budgets derived from
     them (`interactionTimeout`, `deferredVoiceTimeout`, `deferredPurgeTimeout`) are constants.
   When a constant is derived from a knob — as the deferred AI budget is derived from
   `LITELLM_REQUEST_TIMEOUT` — say so at *both* ends, including in values.yaml, or the operator
   tunes one and is surprised by the other.
6. **Output token budgets are completeness knobs, not cost knobs.** A model that exhausts
   `max_tokens` stops **mid-sentence and returns HTTP 200 with no error**. Every budget is sized to
   the Discord surface it renders into (2000-char message, 4096-char embed). When a reply may be
   truncated, say so with `markTruncated`, and use the `discordfmt` chunking helpers rather than
   silently dropping the remainder.
7. **Prompts live in [internal/prompts](internal/prompts/).** No prompt or system message is
   assembled inline in a handler or a worker job.
8. **The AI proposes; the DM decides.** Campaign-state extraction writes *proposals* that a human
   approves through `/review-session`. Nothing the model infers is written to canon automatically,
   and no command invents campaign facts that are not in the record.
9. **Log structurally, with the correlation ID.** Use `logging.FromContext(ctx, g.log)` so the ID set
   at the gateway follows the work through the queue into the worker. Never `fmt.Println`. Errors
   shown to users are short and actionable; the technical detail goes to the log line.
10. **Never print, log, or commit a secret.** `DISCORD_TOKEN`, `LITELLM_API_KEY`, and the storage
    credentials arrive through the environment (External Secrets in-cluster) and stay there.
11. **Never run `cmd/gateway` against the production bot token.** A second live gateway connection
    duplicates command handling and can hijack the voice session of a table that is mid-game.

## 4. Where things go

**Adding a slash command:** define it in `allCommandSpecs()`
([internal/gateway/commands.go](internal/gateway/commands.go)); add a `case` to `routeCommand`
([internal/gateway/interactions.go](internal/gateway/interactions.go)); write the handler in the
matching `handlers_*.go`; if it must work in DMs, add it to `dmCapableCommands()`. `/help` renders
itself from the specs and a test asserts it covers every command, so there is no separate help text
to update — but a command with no options and a vague description produces a useless help entry.

**Adding a queue job:** add the `JobType` constant and its payload struct in
[internal/queue/queue.go](internal/queue/queue.go), a `case` in `Worker.process`
([internal/worker/worker.go](internal/worker/worker.go)), the enqueue call site, and a metrics label.
Decide explicitly which failures are `queue.Permanent`.

**Adding a metric:** [internal/metrics/metrics.go](internal/metrics/metrics.go), next to its
neighbours. Any new failure mode worth alerting on gets a counter — `metrics.ComponentError` for
dependency failures.

**Handler bodies stay disgo-agnostic.** Replies, deferrals, followups, embeds, and option reads all
go through the `ictx` helpers. If you need a new Discord interaction shape, add a helper rather than
reaching for the raw event in a handler.

## 5. House style

The distinguishing feature of this codebase is that **comments explain why, not what** — a knob's
comment says what breaks at the wrong value, a constant's comment says which failure it prevents, and
a workaround names the upstream bug it works around. Match that. A comment that restates the code is
noise; the rationale behind a number or an ordering is the part a future reader cannot recover.

- Errors wrap with `%w` and context: `fmt.Errorf("apply migration %s: %w", name, err)`.
- Exported identifiers are documented; the `revive` exported rule is disabled, but the convention
  holds by practice.
- Tests are plain Go unit tests beside the code they cover, with a comment on each test saying what
  invariant it protects. They must run with **no PostgreSQL, Redis, S3, or Discord** — the suite is
  pure logic. When a change cannot be covered that way, say so rather than adding a hidden
  dependency on a live service.
- `gofmt` and `goimports` decide formatting arguments.

## 6. Git conduct

**Work on a branch and open a pull request against `main`. Every change is reviewed before it
lands.** `main` is what the live bot deploys from, so nothing reaches it directly.

- Branch from an up-to-date `main` as `<type>/<slug>`, e.g. `fix/prep-deadline-exceeded`.
- **Never commit to `main`. Never force-push. Never rewrite published history.**
- One task, one branch, one PR. Open it with the `gh` CLI and **fill in
  [the pull request template](.github/pull_request_template.md)** — every section, in that
  structure. The operator reviews from the PR, not from the chat log, so a claim that only exists in
  conversation does not exist. `gh pr create` does not apply the template automatically, so read the
  file and write the body from it (`--body-file`).
- Do not reformat or "tidy" files the task does not touch — an unrelated diff hides the real change.
- Update the README, [values.yaml](charts/discord-dnd-bot/values.yaml), and the configmap in the same
  commit as the code when you add or alter a configuration knob. A doc update in a follow-up PR is a
  doc update that does not happen.

## 7. Evidence, not assertion

"It works" is not evidence. A test transcript, a log excerpt, a measured duration, or the exact
command you ran is. The PR template's Verification section is where it goes, and its checkboxes are
ticked only for checks that actually ran.

- **Never claim a check that did not run.** If the race suite was not run, say so.
- **Report failures faithfully** — which test, which line, what the output said.
- Distinguish measured from estimated. A latency budget you reasoned about is not a latency you
  observed.

## 8. Repository map

```
cmd/gateway            Discord gateway process: commands, voice capture, reminders
cmd/worker             queue consumer: transcribe, summarize, extract, embed, art
internal/audio         Opus decoding, per-speaker mixing, WAV encoding
internal/config        every environment knob, with its rationale
internal/db            pgx pool, embedded migrations, store_*.go data access
internal/dice          dice-notation parser and roller
internal/discordfmt    Discord length limits, chunking, truncation
internal/extract       validates AI state-extraction JSON into reviewable proposals
internal/gateway       interactions, handlers, ictx helpers, voice manager
internal/httpserver    /healthz, /readyz, /metrics
internal/litellm       OpenAI-compatible client: chat, embed, transcribe, image
internal/logging       slog factory, context loggers, correlation IDs
internal/metrics       Prometheus collectors
internal/prompts       every system/user prompt the bot sends
internal/queue         reliable Redis job queue, payloads, permanent-failure marker
internal/storage       S3-compatible object storage
internal/worker        one file per job type
charts/discord-dnd-bot Helm chart, dashboards, configmap
docs/                  design notes
```

## 9. When to stop and ask

Stop and report rather than guessing when:

- the change would alter recording behaviour (`/session start`, `/session stop`, the voice manager,
  the checkpoint loop) in any way beyond what was asked — that path is load-bearing on game night;
- a schema change would require rewriting or backfilling existing campaign data;
- the task needs a new external dependency, a new backing service, or a new model route;
- a fix would mean weakening a CI check, a linter rule, or a test rather than fixing the cause;
- the request contradicts something in this file, so the contract can be changed deliberately instead
  of quietly.
