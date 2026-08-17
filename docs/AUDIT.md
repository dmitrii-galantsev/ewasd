# System audit and replacement decision

## Executive conclusion

The recurring breakage is not one `already exists` branch. The current system
has no durable answer to three questions: **what is owned, what exact operation
was approved, and how an interrupted write should recover**. It reconstructs
ownership from a source tree, current symlinks, generated ignore files, Git
remotes, and directory names. Those signals drift independently.

Patching another branch in `add_files` would preserve the underlying failure
mode. The Go implementation was therefore built beside the current package with
a smaller feature set and stronger invariants before being promoted to ewasd v1.

## Existing system findings

### Critical safety and consistency issues

1. **Adoption is a non-transactional move.** `shutil.move(local, source)` is
   followed by `symlink_to(source)`. A crash, permission error, or destination
   race between those calls can leave the expected local path absent.
2. **Removal reverses the order.** The local link is unlinked before the source
   is sent to an optional external `trash` executable. Failure leaves the
   source alive but the working path unexpectedly absent.
3. **No cross-process lock or revision check exists.** CLI, library, and MCP
   callers can inspect one state and mutate another. `editors.toml`, ignore
   files, links, and central files can be written concurrently.
4. **There is no journal.** After interruption, neither code nor user can tell
   whether an operation should be completed or rolled back.
5. **Path input is not constrained to the checkout.** Paths containing `..`,
   absolute paths, symlinked parent directories, and special file types are not
   comprehensively rejected before source and target paths are constructed.

### Ownership and drift issues

6. **The source tree doubles as a manifest.** Any top-level central entry is
   treated as desired. Nested targets are inferred recursively and may switch
   between whole-directory and per-file links depending on destination shape.
7. **Existing symlinks are accepted without proving ownership in several link
   branches.** A foreign or stale symlink can be reported as linked.
8. **The known `already exists centrally` case is ambiguous.** It conflates a
   healthy existing link, an equal local copy, divergent content, a wrong link,
   and a dangling link. The current API mostly skips all of them.
9. **Health is incomplete by construction.** Expected links are enumerated from
   sources that still exist, so deleted sources disappear from the expected set.
   The API adds a top-level orphan scan, but nested orphan coverage remains
   inconsistent with recursive linking.
10. **Absolute symlinks encode the old workspace location.** Relocation requires
    an unbounded tree scan and string replacement rather than replaying a known
    manifest.

### Repository identity and Git integration issues

11. **Identity is a basename heuristic.** Remote URL parsing discards host and
    owner. Path components take precedence in monorepos. Two unrelated
    `project` repositories are indistinguishable, and a renamed remote no longer
    matches an existing `editor_workspaces` entry. The live audit reproduced
    this: the checkout remote resolves to `ewasd`, while the configured entry is
    `editor_workspaces`, so `ewasd list` fails despite the project being known.
12. **Fallback auto-creation amplifies a bad guess.** If detection fails,
    adoption may create a persistent repository entry from the leaf directory
    name and an example URL.
13. **`core.excludesFile` is a singleton user setting.** ewasd replaces it with
    a generated file. Worktree-specific handling adds more global Git config
    mutation and still leaves difficult shared-state cases.
14. **Monorepo ignore consolidation walks a mutable working tree.** It combines
    generated files whose stale lifetime and ownership are not tracked.
15. **The `clean` wrapper runs `git clean -x -f`.** A config-overlay tool should
    not own a general destructive cleanup command. Recovery by reconciliation is
    safer than coupling lifecycle to a clean wrapper.

### API, packaging, and maintenance issues

16. **One 1,078-line core module mixes domain state, terminal output, Git
    subprocesses, TOML writes, filesystem mutation, migration, trash, and
    detection.** The structured API captures side effects from this CLI core
    rather than owning a pure domain model.
17. **The MCP server supports two SDK generations by runtime reflection.** That
    compatibility surface is larger than the overlay engine and makes protocol
    behavior package-version dependent.
18. **A clean checkout does not run tests with plain `pytest`.** The audit got
    ten import errors until `PYTHONPATH=.` was supplied. The configured lint
    command also currently fails on `PLR0917`, and `mypy` is declared but was not
    installed in the active environment.
19. **Tests encode unsafe behavior.** For example, “file exists both places” is
    expected to return success while silently leaving the local file unmanaged.
    Most tests mock Git writes and do not inject failures between filesystem
    phases.

## Replacement invariants

The new engine is built around invariants that can be asserted in tests:

1. A destination is managed only if it appears in the versioned state manifest.
2. A registered checkout root is explicit and canonical; remote identity is
   metadata, never an implicit mutation selector.
3. Every input destination is a clean relative path under that checkout, and no
   existing parent may be a symlink.
4. A normal file, directory, foreign link, special file, or divergent source is
   a conflict. There is no force flag.
5. Source content is durable before a destination is replaced.
6. Every mutation runs under one cross-process state lock and checks the state
   revision shown in its plan.
7. A transaction journal is durable before the first destructive rename.
8. A new link is created at a temporary sibling and renamed into place.
9. Manifest persistence uses write + file sync + atomic rename + directory sync.
10. Detach materializes content and archives the source. No normal command
    deletes user content.
11. Git ignore content outside the project's marked block is byte-for-byte
    preserved. `core.excludesFile` is never read or changed.
12. Reconcile creates missing owned links only. It never resolves a conflict by
    overwriting.

## Open-source research

Research used GitHub CLI against primary project repositories on 2026-08-17.

- [chezmoi](https://github.com/twpayne/chezmoi) (21k+ stars): separate source,
  target, and actual state; explicit `diff`, `apply`, `verify`, and merge paths.
  Borrow desired-state verification and source/target separation, not its large
  template/hook/secret ecosystem.
- [GNU Stow](https://github.com/aspiers/stow): preflight conflicts and remove
  only provable ownership. Borrow plan-before-apply; avoid tree folding.
- [Dotter](https://github.com/SuperCuber/dotter) (1.9k+): dry-run, diff, cache,
  and deployment separation. Avoid force-overwrite and templating.
- [yadm](https://github.com/yadm-dev/yadm) (6.3k+): Git as an understandable
  source-history layer. Keep Git optional and outside filesystem transactions.
- [LazyDot](https://github.com/dark-cli/lazydot): exact-path ownership and a
  deliberately narrow feature scope validate abandoning implicit tree scans.
- [DotState](https://github.com/serkanyersen/dotstate): automatic backups and a
  visual workflow are useful, but package management, tokens, profiles, and Git
  hosting integration create a much larger failure and security surface.
- [restic](https://github.com/restic/restic) (35k+): inspectable snapshots,
  explicit checks, and separation of retention from destructive pruning inform
  the archive and activity model.
- [Jujutsu](https://github.com/jj-vcs/jj) (31k+): an operation log makes recovery
  a product concept instead of an implementation detail. The replacement keeps
  a bounded activity log plus durable in-flight journals.
- [Syncthing](https://github.com/syncthing/syncthing) (87k+): a single local
  daemon serving an embedded responsive UI is operationally simple. The new web
  console uses the same engine as the CLI, but exposes no generic filesystem or
  command-execution API.
- [trovl](https://github.com/sneha-afk/trovl): its explicit JSON link manifest
  and separate `plan` (`apply --dry-run`) command independently reinforce the
  plan/apply boundary. Its direct arbitrary-path linker is intentionally broader
  than this repository-scoped replacement.

The requested Kagi helper was not registered as an OpenCode skill, but its local
search helper was discovered and used. It surfaced trovl's current plan/apply
design and local web file-manager comparisons; GitHub CLI and upstream primary
documentation were then used to verify the relevant designs.

## UX decision

The UI is an **operations console**, not a file manager:

- desktop: persistent repository rail, health summary, entry table, and activity
  context;
- every write begins in a preview dialog that names the current
  object, expected state revision, conflicts, and non-overwrite guarantee;
- errors remain next to the action and include a recovery instruction;
- status never relies on color alone, focus is visible, live updates use ARIA,
  and reduced-motion preferences are respected.

Generic path browsing, source editing, uploads, terminal access, Git push, and
deletion were dropped. They add broad remote power while doing little to make
link ownership reliable.

## Parallel evaluation criteria

Before replacing ewasd, use disposable clones and require:

- all unit, race, HTTP contract, and crash-recovery tests pass;
- every mutation remains recoverable after forced interruption at each journal
  phase;
- conflicts never change local bytes;
- a stale browser tab cannot apply a plan after the revision changes;
- existing `.git/info/exclude` content survives all operations;
- desktop layouts show no horizontal overflow, clipped actions, console errors,
  or inaccessible controls;
- the old ewasd data root and every ewasd-owned path remain unchanged.

## Independent review closure

An independent Claude Opus review was run after the first implementation. It
reproduced three release-blocking defects and several correctness gaps. The
parallel implementation was changed before final verification:

- detach recovery now hashes source and materialized content and never removes
  a diverged target; uncertain targets keep their journal for manual recovery;
- adoption recovery no longer reports success around a foreign replacement,
  and copied sources are retained under `recovery/`;
- transient in-checkout backups are added to an exact private Git exclusion
  before they can appear, and final target content is compared with the durable
  source before commit;
- paths with line breaks or edge whitespace are rejected and Git pattern
  metacharacters are escaped; tests use real `git check-ignore` to prove a
  neighboring file is not hidden;
- a deterministic plan fingerprint binds the concrete step/conflict set.
  Reconcile aborts if any additional filesystem drift appears after review;
- the web API now always requires a generated 256-bit token, validates an
  explicit Host allowlist, checks browser origin/fetch metadata, consumes a
  one-use plan ID, and supports TLS directly;
- unresolved journals can be explicitly archived only after confirmation,
  without touching source or target paths, so recovery cannot wedge all future
  writes permanently;
- recovery outcomes are recorded in the manifest activity log;
- exact Git exclude bytes and mode are preserved outside the managed block;
- symlink creation uses `symlink(2)` with EEXIST semantics rather than
  rename-over, overlapping checkout roots are rejected, registration IDs are
  allocated under the state lock, Git subprocesses have timeouts, and graceful
  shutdown waits for active requests;
- crash tests now cover every persisted adopt/detach/reconcile phase, distinct
  Store instances exercise the OS file lock, and a full lifecycle asserts that
  legacy ewasd state and `.ewasd_gitignore` remain unchanged.

At that review stage the implementation remained a **parallel evaluation
build**, not an automatic migration. Residual platform-level TOCTOU risk from
an unrelated process replacing path ancestors between validation and syscalls
cannot be eliminated portably without Linux-specific `openat2`/descriptor-walk
operations. The engine therefore targets trusted single-user workstations,
holds its own cross-process lock, fails on every observed collision, and keeps
the old system isolated while this design is evaluated on disposable clones.

A targeted Opus follow-up reproduced the remediations and changed its then-current gate to
**GO for disposable clones and GO for repositories with uncommitted work**, while
still rejecting production migration. It found two non-destructive P2
regressions during that pass; both were then closed: recovery now commits one
journal/event per transaction so a later blocked journal cannot erase the audit
record of earlier completed recovery, and Git exclude separator metadata is
recognized only inside this project's marker block, preserving an identical
user line byte-for-byte. Regression tests cover both cases.

The final completion pass added the missing browser release gate rather than
leaving the manual Chrome evidence as a one-time claim. A Playwright fixture now
builds the real binary, creates two temporary Git repositories, registers and
adopts through the CLI, serves the embedded UI with mandatory authentication,
and exercises real Chromium at standard and 1440p desktop sizes.
The suite checks overflow, 44px targets, console/page errors, axe WCAG findings,
repository switching, activity and safety screens, modal accessibility,
blocked no-op plans, plan/apply/activity flow, and rejection of a reconcile plan
whose filesystem step set changed after review. CI runs these tests and uploads
screenshots/traces; a separate Nix job builds the independent flake.

## Promotion and Python-to-Go migration

After the parallel gate passed, the Go implementation was promoted to the
repository root and the Python package, Python tests, setuptools metadata, and
Python-oriented CI were removed. The migration design intentionally does not
reinterpret all files in the old central source tree:

1. Parse the narrow legacy `editors.toml` schema with a rejecting Go parser.
2. Scan only explicit roots for files carrying the exact generated
   `.ewasd_gitignore` header.
3. For each listed live symlink, require its resolved target to sit beneath a
   configured `link_dir`.
4. Infer the checkout scope by subtracting that exact source-relative path from
   the target path, then validate the resulting Git root and overlap rules.
5. Copy only live managed entries into the new versioned profile. Legacy source
   bytes remain untouched.
6. Commit the new manifest before switching links. Every switch has a durable
   journal and preserves the old link as a sibling backup until health and the
   new private Git exclude block are verified.
7. Archive each generated marker, unset `core.excludesFile` only when it points
   exactly to that marker, and remove the marker.
8. Write a migration receipt and leave the old Python workspace intact as a
   rollback source.

`scripts/replace-python-with-go.sh` builds and activates Go ewasd before
mutating any legacy links. Nix installations first attempt `nix profile upgrade`; when
an old locked or raw-store element cannot upgrade, Go ewasd is added at higher
priority and verified before the hidden Python element is removed. Thus the
command is never absent and both profile generations remain rollback points.
The fixture's copy mode uses a staged executable and atomic rename. CI exercises
the complete fixture.

The installer also avoids a subtle Nix disclosure hazard: `path:<checkout>`
copies ignored and untracked files into the world-readable Nix store. The
replacement script constructs a persistent allowlisted source tree containing
only flake metadata, `go.mod`, `cmd/`, and `internal/`, and builds/installs that
snapshot instead of the checkout.

### Migration review closure

A third Opus gate focused specifically on the real Python-to-Go migration and
initially blocked execution. Its findings drove additional changes:

- shared legacy `link_dir` values now produce one `SourceID`, preserving
  cross-worktree sharing instead of silently splitting it;
- marker finalization is grouped by Git common-dir, preserves residual ignore
  semantics, and uses its own resumable synced journal;
- hand-written foreign marker files are skipped rather than aborting a scan;
- the Nix package explicitly runs `go test ./...` instead of testing only the
  command package;
- Go is activated before link mutation, mode combinations are preflighted, and
  both raw-store and flake profile replacement paths are tested without a
  command-absence window;
- the installer uses a private allowlisted source rather than leaking ignored
  checkout files into the Nix store.

The follow-up review reproduced every fix and issued an explicit **GO**. It ran
34 kill-at-phase trials, verified the real shared OrcaSlicer topology, copied the
real 63 MB `.opencode` shape with contained symlinks, confirmed zero content
loss, and exercised the real Nix profile fallback. Two non-blocking observations
from that pass—second-wave marker finalization and a transient resumed-plan
verification false negative—were subsequently fixed and regression-tested.

## Reintroduced workflows in v1.1

Repository guessing and `ewasd clean` were restored after the manifest and
recovery model was stable:

- guessing no longer treats a basename as unconditional mutation authority;
  it returns a trace and blocks ambiguity, preferring exact roots and normalized
  remote+scope matches before path-component fallback;
- inferred checkouts bind to an existing shared `SourceID`; missing links are
  created with EEXIST semantics and ordinary-file conflicts are untouched;
- `clean` retains the useful workflow but is preview-first; apply requires the
  reviewed revision and fingerprint, while broad ignored/directory cleanup is
  explicit;
- instead of trusting ignore state, clean supplies exact `-e` protection for all
  ewasd blocks and manifest entries while using one `-f`, a bounded project
  pathspec, no arbitrary argument forwarding, and post-clean link verification.
