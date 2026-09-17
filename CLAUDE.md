# CLAUDE.md

**Read [AGENTS.md](AGENTS.md) first.** It is the operating contract for this repository and it is not
repeated here. This file adds only what is specific to Claude Code.

## How work arrives

The operator hands over one task at a time, in conversation — there is no issue queue. So the brief
is the message, and anything it does not settle is a judgement call to make explicitly rather than
silently. Restate the shape of a non-trivial change before writing it, and say which files it will
touch.

When a task turns out to be larger than it looked, finish the part that was asked and **name what was
left out and why**. Do not widen a change on your own initiative: an unrelated refactor buried in a
bug fix is the diff the operator cannot review.

## The environment

- WSL2 under VS Code. Paths are Linux paths; the repository is at `~/discord-dnd-bot`.
- **The Go toolchain is not on `PATH` by default.** Start with:
  `export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin` — that also picks up `golangci-lint`,
  `govulncheck`, and `gosec`.
- `CGO_ENABLED=1` and `libopus-dev` are required for anything that builds the voice path. The cgo
  build prints a wall of `-Wstringop-overread` warnings from the vendored Opus C sources; they are
  pre-existing and not yours.
- Prefer the file and search tools over shelling out for reads and edits.

## Verification before reporting

Run the checks, then say what you ran. The loop is:

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
```

There is no way to exercise a Discord interaction end to end from here — no test harness fakes the
gateway, and the live bot must not be run from a development machine. That means changes to handlers,
voice, or timeouts are verified by reasoning plus unit tests, and the honest report says so. When a
deadline or a budget changes, add the assertion that pins it (see
[interactions_timeout_test.go](internal/gateway/interactions_timeout_test.go)) rather than leaving
the invariant in a comment.

## Diagnosing production behaviour

The operator usually arrives with a JSON log line. Read it precisely before theorising:

- `duration_ms` landing repeatedly within a few milliseconds of a constant in the code means a
  deadline fired, not that a dependency was down.
- `correlation_id` ties a gateway interaction to its queued job and the worker that ran it — follow
  it rather than guessing which log lines belong together.
- `source` in every line gives `file` and `line`; go there first.

## GitHub operations

Use the locally authenticated `gh` CLI. Never ask for a token, never print one, never write one to a
file. Feature-detect an unfamiliar flag with `--help` rather than inventing it.

Work lands through a pull request against `main` (AGENTS.md §6), so finishing a task means: branch,
commit, push, `gh pr create`, and hand back the PR link. Do this as the last step of the task — it is
part of finishing, not a separate request to wait for.

`gh pr create` does not apply the repository's PR template, so read
[.github/pull_request_template.md](.github/pull_request_template.md) and write the body from it with
`--body-file`. The verification evidence goes there; the chat transcript is not part of the record
the operator reviews.

## Deploying

Deployment is ArgoCD-driven from the chart in [charts/discord-dnd-bot](charts/discord-dnd-bot/) —
Claude does not deploy, roll back, or `kubectl` against the cluster. Describe the change the operator
needs to make and let them make it.
