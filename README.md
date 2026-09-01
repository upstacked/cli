# Upstacked CLI

`ups` operates Upstacked infrastructure from the terminal: devices, monitoring,
credentials, IPAM, changes, runbooks, tickets, and infrastructure-as-code.

```sh
brew install upstacked/tools/cli
ups init --api-url staging
ups doctor
```

## Getting started

`ups init` walks through the server, login, default infrastructure, and installs
the agent skill. Every step is also a standalone command, so nothing it does is
hidden:

```sh
ups login                  # authenticate (password from stdin, never argv)
ups context set            # pick a default infrastructure
ups skill install          # install the LLM agent skill
ups doctor                 # verify all of it; non-zero exit if anything is wrong
```

`ups context show` reports the active server and infrastructure **and where each
value came from**. Staging and production differ by one line of config and
nothing in the prompt, so check it before you write.

## Everyday use

```sh
ups event list                       # what is broken right now
ups change log --since 24h           # what changed recently
ups host list                        # devices in the active infrastructure
ups maintenance create --hosts 12,13 --duration 2h --name "firmware"
ups ipam next --subnet 7 --claim     # take the next free address
ups runbook preflight 5              # check credentials before running
ups logs search --since 1h --text "link down"
```

## Infrastructure as code

Export an infrastructure to YAML, keep it in git, and reconcile:

```sh
ups export --out ./infra/   # one file per host
ups diff ./infra/           # read this. every time.
ups apply ./infra/
```

`--out` takes a directory or a single `.yaml` file. Prefer a directory on a
real infrastructure — one file per host keeps git diffs small:

```
infra/
  infrastructure.yaml
  hosts/
    core-sw-01.yaml
    fw-01.yaml
```

Re-exporting into a directory removes host files whose resource is gone from
the platform, and reports which. A leftover file would read as a host to
create on the next apply.

`apply` is idempotent and safe to re-run. It is not safe to run unread: the
export covers a whole infrastructure, so a block missing from the document is a
deletion. Deletions require `--allow-delete`.

Exported documents carry an `id:` per host and monitoring item. Keep it and a
name change is a real rename — one update, history intact. Strip the ids and
matching falls back to name, where a rename is indistinguishable from
delete-plus-create; `ups diff` warns when a create/delete pair looks like an
accidental rename. Drop ids deliberately when you want a portable template for
a different infrastructure.

## Scripting

```sh
ups host list --json | jq '.items[].name'
ups host list --id-only | xargs -n1 ups host show
UPSTACKED_TOKEN=... ups doctor --json     # no config file needed
```

- `--json` is the stable interface. Table output is not; do not parse it.
- Exit codes: `0` ok, `1` general, `2` usage, `3` auth, `4` not found, `5` conflict.
- Truncated result sets are always reported. Truncated is not "no more matches".
- `--dry-run` previews mutating commands; destructive ones confirm unless `--yes`.

## Configuration

Precedence, highest first:

| Layer | Example |
|---|---|
| flag | `--api-url https://…`, `--infra 42` |
| environment | `UPSTACKED_API_URL`, `UPSTACKED_TOKEN`, `UPSTACKED_INFRASTRUCTURE` |
| profile | `~/.config/ups/config.yaml` |

Profiles keep servers apart. Credentials are stored per profile at `0600` and
**bound to the server that issued them** — the CLI will not send a token to a
host it was not issued for. An auth failure after switching servers is that
safeguard working; run `ups login` against the new host.

`--api-url` accepts short aliases (`staging`) as well as full URLs.

## The agent skill

The binary embeds an LLM skill and installs it with `ups skill install`
(by default to popular agents: Claude, Cursor, Codex and Gemini, under
`~/.<agent>/skills/upstacked/`, or `--scope project`). It explains the resource
model and, more importantly, *why* the workflows are shaped as they are — an
agent can read `--help` for the how, but cannot derive from any API response
that deleted monitoring fails silently.

`ups doctor` reports whether the installed skill is missing, outdated, or
locally edited, and never overwrites your edits without `--force`. CI fails if
the skill names a command that does not exist.

## Development

```sh
go test ./...          # integration tests drive the real command surface
go build ./cmd/ups
```

Tests run commands end to end against a stub API and assert on stdout, stderr,
and exit code. The layering — transport, resource model, diff engine, renderer,
command layer — is what keeps that possible.

See [docs/user-stories.md](docs/user-stories.md) for the backlog and the
reasoning behind each guardrail.

## Releasing

Push a tag; GitHub Actions builds all platforms and updates the Homebrew tap.
See [docs/releasing.md](docs/releasing.md).

## License

MIT
