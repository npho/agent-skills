# ~/.agents — managed agent skills

This directory is the single source of truth for your installed agent skills.
It is a git repository. On any new machine, cloning this repo and running
`skl sync` reproduces your exact skill setup.

## Layout

| Path | Tracked? | Owner | Purpose |
|---|---|---|---|
| `.skill-lock.json` | yes | `npx skills` (skl only reads it) | Manifest of skills installed via the `skills` CLI: source repo, path within it |
| `.sync-state.json` | yes | `skl` (never hand-edit) | Per-skill commit pins, file hashes, override flags |
| `overrides/<skill>/` | yes | you (via `skl promote`) | Your promoted modifications, applied on top of pristine upstream |
| `skl/` | yes (source only) | you | The `skl` Go tool source + Makefile |
| `bin/` | **no** (git-ignored) | `make` | Built `skl` binary |
| `skills/<skill>/` | **no** (git-ignored) | `skl sync` | Disposable cache: pristine upstream + overrides. Rebuildable at any time |

**Invariant:** `skills/` is always `upstream @ pinned commit + overrides applied`.
It can be deleted and rebuilt from this repo alone. Never commit anything under
`skills/` — `skl check` enforces this (guard error) if it ever sees a git-tracked path there.

## Building the binary

Requires a Go toolchain (only at build time — the binary itself is dependency-free):

```sh
cd ~/.agents/skl && make      # builds ~/.agents/bin/skl
```

Optionally put it on your PATH (e.g. `ln -s ~/.agents/bin/skl ~/bin/skl`).

Every command accepts `-n` / `--dry-run` (anywhere on the command line) to preview
actions without changing anything. `skl` **never commits to git** — you commit manually.

## Commands

```
skl add <owner/repo | github-url | /local/dir>   install a skill, then track it
skl sync                                         install missing skills, apply overrides,
                                                 fix ~/.pi symlinks, refresh state
skl check                                        verify everything (exit 1 on problems)
skl update [skills...]                           update skills, re-pin commits, re-hash
skl promote <skill>                              move local edits into overrides/
skl help
```

## Workflow

### Adding a new skill

```sh
skl add mattpocock/skills          # or: skl add https://github.com/owner/repo
                                   # or: skl add /path/to/local/skill-dir
skl check                          # verify the install
git add .skill-lock.json .sync-state.json && git commit -m "skill: add <name>"
```

`skl add` runs `npx skills add` (the `skills` CLI remains the sole writer of
`.skill-lock.json`), then records the skill in `.sync-state.json` with its current
upstream commit pinned. Local directories are installed directly and recorded as
"external" skills in `.sync-state.json` (they do not appear in the npx lock).

### Checking for drift

```sh
skl check
```

Reports, per skill: `OK`, `DRIFTED` (with the exact files modified/added/removed),
`MISSING`, `UNTRACKED` (in lock, not in state), orphaned overrides, broken pi
symlinks, and the git-guard. Exit code 0/1 makes it usable in scripts/CI.

### Modifying a skill

1. Edit it in place under `~/.agents/skills/<name>/` — drift shows up in `skl check`.
2. When the edit becomes *official*: `skl promote <name>`.
   This snapshots the modified skill into `overrides/<name>/`, reinstalls pristine
   upstream from the pinned commit, and re-applies your overlay.
3. Commit: `git add overrides/<name> .sync-state.json && git commit`.

From then on, upstream updates and reinstalls always preserve your overlay.
A drift you never promote is always visible in `skl check`, so edits can't be
silently lost.

### Updating skills

```sh
skl update                 # everything
skl update docx xlsx       # specific skills
skl check
git add .sync-state.json && git commit
```

Lock-based skills update via `npx skills update` (Node required); external
GitHub skills are re-fetched directly. Overrides are re-applied automatically;
upstream changes to files you overrode will be masked by your override — review
upstream diffs periodically for promoted skills.

### Bootstrapping a new machine

```sh
git clone <this-repo> ~/.agents
cd ~/.agents/skl && make
~/.agents/bin/skl sync        # -n first if you want to preview
~/.agents/bin/skl check       # expect "all clean"
```

Skills pinned in state install at their pinned commits (reproducible). Skills in
the lock but not yet in state install at upstream HEAD and get pinned on first
sync. Node is **not** required for sync — only for `add`/`update` of lock-based
skills.

## Design rules (why it's built this way)

- **One writer per file.** `npx skills` owns `.skill-lock.json`; `skl` owns
  `.sync-state.json`; you own `overrides/`. skl reads the lock, never writes it,
  so the two tools can't corrupt each other's state.
- **The lock pins paths, not commits.** `.sync-state.json` adds the missing
  reproducibility layer (commit SHA per skill).
- **`skills/` is a cache, not state.** Git tracks intent (lock + state +
  overrides), not upstream payloads — the repo stays small forever.

## Known limitations

- GitHub sources only (plus local directories via `skl add`).
- Overrides are file-level overlays: they replace/add files but cannot delete
  upstream files (delete them directly in `skills/` and promote if needed).
- Unauthenticated GitHub API use for commit resolution (60 req/hr — fine at this
  scale; set `GITHUB_TOKEN` support if it ever becomes an issue).
- `skl add` for npx packages requires Node/npx available.
