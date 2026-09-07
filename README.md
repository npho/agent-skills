# `~/.agents` skill library

This repository is an FHS-inspired, harness-agnostic skill library. `skl` keeps
canonical payloads separate from the small directory a harness scans, and can
materialize reproducible real copies inside projects.

## Layout

| Path | Tracked | Purpose |
|---|---:|---|
| `lib/<namespace>/<skill>/` | no | Canonical installed payload. GitHub uses the owner as namespace; filesystem installs use `local`. |
| `skills/` | no | Harness-visible global exposure. Empty by default; enabled entries are relative symlinks into `lib/`. |
| `etc/profiles/` | yes | Optional named, composable TOML selections; its README is the inactive template. |
| `var/state.json` | yes | `skl` v2 state: canonical path, provenance, revision, timestamps, hashes, and global exposure. |
| `.skill-lock.json` | yes | Metadata owned by `npx skills`; `skl` reads it but never writes it. |
| `skl/` | yes | Go source and tests. |
| `bin/skl` | no | Built executable. |

`SKL_ROOT` overrides the default root (`~/.agents`), including in tests and
automation. No command commits or pushes.

## Build and migrate

```sh
cd ~/.agents/skl
gofmt -w *.go
go test ./...
go vet ./...
make                         # writes ~/.agents/bin/skl
~/.agents/bin/skl migrate    # safe and idempotent legacy migration
~/.agents/bin/skl check
```

Migration reads legacy `.sync-state.json`, verifies each legacy payload against
its recorded hashes, moves it to `lib/<owner>/<skill>`, writes versioned
`var/state.json`, and removes the old state file. It removes only old Pi symlinks
whose resolved target is that known skill's former `~/.agents/skills/<name>`;
real directories and unrelated links are untouched. It can be previewed with
`--dry-run`. The resulting `skills/` is empty unless exposure is subsequently
enabled.

## User-library commands

```sh
skl add owner/repo                  # npx discovers skills; skl adopts into lib
skl add /path/to/a/skill            # installs as lib/local/<skill>
skl add --global owner/repo         # also explicitly expose newly added skills
skl list
skl global list
skl global enable owner/skill
skl global disable owner/skill
skl sync                            # restore missing copies at recorded pins
skl update [owner/skill ...]        # advance directly from original sources
skl check
```

Unqualified names are accepted only when unique. GitHub pins are immutable
40-character commit SHAs; resolution failures stop the operation. `sync` and
`update` refuse to overwrite library drift; use `--force` deliberately. `--dry-run`/`-n` can appear
anywhere and does not write state, payloads, links, manifests, or locks. Output
that `npx skills` temporarily places under `skills/` is adopted into `lib/` and
never interpreted as global exposure.

## Profiles

A profile is `etc/profiles/<name>.toml`:

```toml
extends = ["base", "review"]
skills = ["trailofbits/codeql", "local/team-conventions"]
```

Profiles may extend profiles. Resolution rejects cycles, missing profiles or
skills, ambiguous unqualified names, and two selected canonical skills that
would collide at the same destination name.

```sh
skl profile list
skl profile show security
```

## Projects

A project contains committed, human-authored intent and generated exact state:

- `.agents/skills.toml`: manifest with `version`, `profiles`, and `skills`.
- `.agents/skills.lock.json`: exact source metadata, revision, and file hashes.
- `.agents/skills/<skill>/`: real copied directories, never symlinks.

```sh
cd /path/to/project
skl project init --profile security local/team-conventions
skl project add trailofbits/codeql
skl project refresh                 # explicit profile re-resolution
skl project sync                    # reproduce the existing lock exactly
skl project update [skill ...]      # advance directly from original sources
```

`init`, `add`, and `refresh` seed from a clean canonical library copy when it is
available. Profile changes do not affect an existing project until `refresh`.
`project sync` does not resolve profiles or depend on `lib`; when restoration is
needed it fetches the source and revision recorded in the project lock directly.
`project update` likewise fetches the recorded original source, advances GitHub
revisions, and rewrites copies and hashes. Project changes are fully staged and
validated before the payload tree and metadata are swapped with rollback. Sync
also rejects unmanaged entries. Both refuse to overwrite local edits or remove
unmanaged entries unless `--force` is explicit. Local filesystem provenance remains local in the
lock; GitHub records retain owner, repository, in-repository skill path, and
revision.

Use `--project DIR` to target a project without changing directory. Commit a
project's manifest, lock, and copied `.agents/skills/` according to that
project's own policy.
