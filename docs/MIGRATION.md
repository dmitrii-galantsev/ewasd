# Python-to-Go migration record

The workstation migration was executed on 2026-08-17 with
`scripts/replace-python-with-go.sh` after two Opus reviews and a faithful
rehearsal using the real legacy data shapes.

## Result

- Installed command: `ewasd 1.0.0`
- Installed path: `~/.local/state/nix/profile/bin/ewasd`
- Go state: `~/.local/share/ewasd-v2` (mode `0700`)
- Retained Python workspace: `~/.local/share/ewasd`
- Migrated checkouts: 7
- Migrated link mappings: 52
- Healthy links after migration: 52
- Conflicts, missing links, missing sources, recovery journals: 0
- Generated legacy markers archived and removed: 7
- Python `ewasd-mcp` executable removed with the Python profile package
- Nix profile references to Python/GitHub ewasd: 0

The two OrcaSlicer worktrees retain one shared `SourceID`, so all eight common
files still resolve to the same central source. Detach is intentionally blocked
for those shared entries rather than silently splitting them.

## Data-integrity evidence

- A manifest of all 3,993 legacy workspace files/symlinks was hashed before and
  after migration. Both manifests have SHA-256:
  `0409dde5e24771323c4f26e3a1df89e7f49455cf5f5855b9f4ff3e50c6daaa6c`.
- Every migrated target was checked against its retained legacy source for
  content, type, permissions, directory members, and contained symlink targets.
  All 52 mappings matched.
- `git status --short --untracked-files=all` was captured before migration for
  every affected checkout and compared afterward. All seven statuses were
  byte-identical.
- The four stale orca_rebase paths from the legacy marker remain protected in a
  marked `legacy-residual` block in the shared private Git exclude file.
- The migration was rerun after completion; `state.json` remained byte-identical.

## Receipts and recovery

Durable records are stored under `~/.local/share/ewasd-v2/legacy/`:

- `migration-plan.json`
- `migration-result.json`
- `migration-receipt.json`
- `replacement-receipt.txt`
- `finalization.json`
- `markers/*.ewasd_gitignore`
- `installer-source-*/` for the active Nix profile's durable local flake source

The old workspace was intentionally not deleted. It is a byte-identical rollback
source, but no live migrated link points to it.

## Post-migration UI verification

The installed binary served the real seven-project state and was checked with
Chrome MCP and Lighthouse:

- no horizontal overflow;
- no visible target below 44×44 CSS pixels;
- no console warnings/errors;
- all seven projects available with duplicate worktree names disambiguated;
- Lighthouse: 100 accessibility, 100 best practices, 100 SEO, and 100 agentic
  browsing.
