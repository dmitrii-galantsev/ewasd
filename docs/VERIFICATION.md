# Verification record

This record maps the Go replacement and Python-to-Go migration to reproducible
evidence. The former Python implementation has been removed from the product
tree; its Git history and retained user workspace remain rollback sources.

## Objective coverage

| Requirement | Evidence |
|---|---|
| In-depth audit of recurring failures | `AUDIT.md` documents safety, ownership, identity, Git integration, API, packaging, and maintenance failures in the Python implementation. |
| Critical rearchitecture allowed | The replacement is an independent Go module with an explicit manifest, plan fingerprints, locks, recovery journals, CLI, and embedded web console. It shares no code or state with ewasd. |
| Full migration | `migrate-legacy` parses the Python workspace, discovers only generated marker inventories and exact live symlinks, copies source bytes, switches links, verifies health, archives markers, and retains the old workspace. |
| Command replacement | `scripts/replace-python-with-go.sh` builds Go ewasd, applies migration, verifies empty journals, and activates Go in the Nix profile before removing Python. A copy-mode fixture validates the same replacement logic without touching the user profile. |
| Open-source research | `AUDIT.md` records GitHub/Kagi research into chezmoi, GNU Stow, Dotter, yadm, LazyDot, DotState, restic, Jujutsu, Syncthing, and trovl. |
| Terra subagents | Independent Terra sessions covered architecture failure modes, open-source design research, and desktop product design without blocking implementation. |
| Opus consultation and review | Opus performed two adversarial reviews. Reproduced P0/P1 findings and the two follow-up regressions are recorded with their remediations in `AUDIT.md`. |

## Reproducible gates

Run from the repository root:

```bash
go test -race ./...
go test -race -count=20 ./internal/engine ./internal/store ./internal/httpapi
go vet ./...
test -z "$(gofmt -l .)"
nix flake check --no-build
nix build
```

The replacement fixture is also a release gate:

```bash
bash tests/replacement/run-fixture.sh
```

## Verified safety properties

- Inputs cannot be absolute, contain traversal, line breaks, edge whitespace,
  Git control components, special files, or symlinked parents.
- The central source path is independently contained beneath the Go data root.
- Overlapping checkout roots and duplicate registrations are rejected.
- Every write is serialized by an OS file lock and checks a manifest revision.
- A plan fingerprint binds the exact step/conflict set; web plans additionally
  use a one-use, ten-minute `plan_id`.
- `symlink(2)` EEXIST semantics prevent rename-over of a path that appeared
  after planning.
- Adoption makes the source durable first and retains an excluded rollback
  backup until the manifest is committed.
- Detach and recovery compare tree digests before removing a materialized copy;
  diverged user data is retained and blocks automatic recovery.
- Recovery validates journal paths, records each completed journal in its own
  manifest transaction, and supports explicit journal-only archival when
  certainty is impossible.
- Git pattern metacharacters are escaped and real `git check-ignore` tests prove
  neighboring files remain visible.
- User bytes and mode outside the marked `.git/info/exclude` block round-trip
  exactly; malformed/duplicate markers stop the write.
- The web API always requires a generated 256-bit token and approved Host,
  rejects cross-site browser writes, caps bodies, and serves a restrictive CSP.

## Repository inference and clean

- Detection precedence is explicit override → deepest registered root → unique
  normalized remote plus registered monorepo scope → unique registered path
  component. Ambiguous source profiles are returned as candidates and block the
  write.
- `ewasd link --dry-run` exposes the detection method, exact target root, links,
  conflicts, and Git-exclude change. The default apply never replaces normal
  files or foreign links; conflicts remain visible in the registered manifest.
- New checkouts share the registered `SourceID`, so inferred worktrees do not
  fork central files.
- `ewasd clean` runs one-force `git clean` only inside the detected project
  scope. It passes exact exclusions for all ewasd blocks in the shared private
  Git exclude file plus every expected manifest entry.
- Nested Git repositories are skipped because ewasd never supplies Git's second
  `-f`. Previously healthy managed links are verified after clean.
- Clean modes (`all`, `untracked`, `ignored`), directory behavior, quoted/newline
  paths, monorepo scope containment, residual patterns, conflicts, remote
  ambiguity, and shared sources are covered by real-Git integration tests.

## Migration safety properties

- Discovery reads only generated `.ewasd_gitignore` inventories under explicit
  scan roots; it does not infer ownership from arbitrary untracked files.
- Every candidate must be an exact symlink into a configured legacy source.
- Project roots are derived by subtracting the exact source-relative path from
  the target, then checked against Git and overlap invariants.
- Checkouts that referenced the same legacy `link_dir` retain one shared Go
  `SourceID`; linked worktrees therefore continue to observe the same edits.
- Missing stale marker entries are reported but do not block migration; unknown
  or unsafe symlinks do block it.
- Legacy source content is copied, never moved or deleted.
- Each link switch has a durable journal and a sibling backup of the old link;
  manifest commit precedes cleanup, so recovery can finish or roll back.
- Generated marker bytes are archived before removal. `core.excludesFile` is
  grouped by Git common-dir and unset once only when its repository-owned origin
  points to a generated marker. Residual stale/non-link patterns are preserved
  in a marked private Git exclude block.
- Marker finalization has its own synced, resumable journal; rerunning
  `migrate-legacy --apply` converges after interruption.
- Post-migration health requires every imported entry linked, zero conflicts,
  zero missing sources, and a verified private Git exclude block.
- The Python workspace remains intact after successful replacement.
- Nix installation is built from an allowlisted source snapshot, not the whole
  checkout, so ignored/untracked local files cannot leak into the Nix store.
