# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

`ups`, the Upstacked CLI. It manages live network infrastructure: real devices, real alerts,
real people who get paged. Most commands touch production.

## Always update the skill

`internal/skill/SKILL.md` ships to the user's LLM clients via `ups skill install`. It is the
only thing an agent knows about this CLI.

**Whenever you add, change, or remove a command, flag, or behaviour, update SKILL.md in the
same change.** A skill that describes a command surface the binary does not have makes an
agent confidently wrong, which is worse than shipping no skill at all.

This is partly enforced — `TestSkillDescribesOnlyRealCommands` and `TestSkillUsesOnlyRealFlags`
fail the build when the skill names something that does not exist — but the tests only catch
drift in one direction. A new command the skill never mentions passes CI and is invisible to
every agent. That direction is on you.

The skill explains **why**, not just how. `TestSkillExplainsTheReasoning` pins the phrases
that carry the reasoning; when you add a feature with a hazard attached, document the hazard
alongside the command rather than listing the flag and moving on.

## Releasing

Releases are cut by pushing an annotated tag to `main`. GoReleaser then builds six
platform archives, publishes a GitHub Release, and updates the Homebrew cask in
`upstacked/homebrew-tools`. History is linear and commits land directly on `main`.

```sh
gofmt -l . && go vet ./... && go test -race -count=1 ./...   # what CI runs
git push origin main
git tag -a v0.0.8 -m "v0.0.8"
git push origin v0.0.8
gh run watch --repo upstacked/cli                            # confirm it published
```

Only tag a commit that is already pushed and green: the tag is what publishes, and a
release cannot be quietly amended once the cask has moved.

Do **not** also run `goreleaser release` locally for the same tag — the workflow and the
local run both upload the same assets and the second fails with `already_exists`. To
rehearse, use `goreleaser release --snapshot --clean --skip=publish`.

The version reaches the binary through ldflags, and the skill's `version=` marker comes
from the same variable, so `ups doctor` reports a skill installed from an older release as
outdated on its own. Nothing about the version is edited by hand.

Full detail, including the Homebrew tap's GitHub App credentials: `docs/releasing.md`.

## The rules the code is built around

Read SKILL.md before changing behaviour. In short:

- **Silent loss of monitoring coverage is the worst outcome.** Nothing pages anyone when a
  check disappears. Anything that removes or replaces monitoring must name what it destroys
  before it writes, and confirm.
- **Never prompt when stdin is not a terminal.** Fail with the flag the caller needed.
- **Secrets come from stdin, never argv.**
- **`--yes` suppresses the prompt, not the safety.** It does not imply `--allow-delete`.

## Conventions

- One file per command family in `internal/cli/`. Generic JSON rows (`row`, `str`, `dash`),
  not hand-written structs — several endpoints declare no response schema.
- `--json` for anything scriptable; never parse table output. Exit codes are meaningful
  (`internal/errs`).
- Tests use the stub server in `harness_test.go` and assert on observable behaviour: what a
  user sees, and which requests were sent. Test the thing that prevents damage.
- Comments explain non-obvious behaviour and the reason a guard exists. Do not narrate code
  that speaks for itself.
