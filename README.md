# ewasd

`ewasd` is an explicit, journaled repository-overlay manager. Version 1 is a
complete Go replacement for the former Python implementation.

The single Go binary has two clients over the same engine:

- a plan-first CLI for scripts and recovery;
- an optional local desktop web console.

Use `scripts/replace-python-with-go.sh` to import exact live Python-era links,
verify them, retire generated legacy marker files, and atomically replace the
installed Nix profile package.

## What changed

The core model is **an explicit manifest**, not a filesystem scan:

- checkouts are registered explicitly; a path component never silently selects
  a profile;
- each managed destination is recorded exactly once;
- every write checks a state revision, obtains a cross-process lock, revalidates
  filesystem preconditions, and writes a recovery journal;
- adoption copies to durable central storage before the local file is replaced;
- normal conflicts are reported and never overwritten;
- detach materializes a regular local copy and archives the central source;
- Git ignores are a marked block in `.git/info/exclude`; user content is
  preserved and `core.excludesFile` is never changed;
- protected, preview-first `git clean` integration is included; shell hooks,
  templates, remote fetching, secrets, and generic file-manager endpoints are
  not.

See [docs/AUDIT.md](docs/AUDIT.md) for the failure analysis and
[docs/VERIFICATION.md](docs/VERIFICATION.md) for reproducible evidence. The
completed workstation migration is recorded in
[docs/MIGRATION.md](docs/MIGRATION.md).

## Install

```bash
# From this checkout. Existing Python links are migrated automatically; on a
# new machine the legacy phase is simply skipped.
./scripts/replace-python-with-go.sh
```

The replacement script is non-interactive. It aborts on ambiguous ownership or
unsafe links rather than prompting or guessing.

## Build and test

```bash
go test -race ./...
go vet ./...
go build -o /tmp/ewasd ./cmd/ewasd
npm ci
npm run test:browser
```

Or build the Nix flake:

```bash
nix build
```

The Playwright suite launches a completely isolated fixture server and tests
two desktop layouts against real Chromium:

- standard desktop (`1440×1000`);
- 1440p desktop (`2560×1440`).

It checks horizontal overflow, 44px control targets, console/page errors,
serious or critical axe accessibility findings, repository switching, blocked
plans, plan/apply/activity behavior, and stale reconcile rejection after
filesystem drift. Responsive screenshots are written to
`browser-artifacts/responsive/`.

The replacement has a separate data root:

```text
$XDG_DATA_HOME/ewasd-v2/         # default: ~/.local/share/ewasd-v2
  state.json                     # versioned explicit manifest
  state.lock                     # cross-process advisory lock
  profiles/<project-id>/files/   # live source files
  archive/<operation-id>/        # detached sources; never auto-deleted
  transactions/                  # crash-recovery journals
```

Override the state location with `EWASD_HOME`:

```bash
export EWASD_HOME=/tmp/ewasd-comparison
```

## CLI workflow

```bash
# Explain which registered source profile matches this checkout and why.
ewasd detect

# Infer from an exact registered root, normalized Git remote + monorepo scope,
# or a unique registered path component. Creates missing links by default.
ewasd link
ewasd link --dry-run

# Register a checkout explicitly. This does not adopt any files.
ewasd register --root /absolute/path/to/repo --name "My repo"

# Read-only orientation.
ewasd status --root /absolute/path/to/repo

# Preview is the default. Nothing is changed.
ewasd adopt --root /absolute/path/to/repo AGENT.md

# Re-run with both the shown state revision and --apply.
ewasd adopt --root /absolute/path/to/repo --revision 1 \
  --fingerprint <fingerprint-from-preview> --apply AGENT.md

# Restore missing owned links, but never overwrite a conflict.
ewasd reconcile --root /absolute/path/to/repo
ewasd reconcile --root /absolute/path/to/repo --revision 2 \
  --fingerprint <fingerprint-from-preview> --apply

# Turn a managed link back into a normal file. The source is archived.
ewasd detach --root /absolute/path/to/repo AGENT.md
ewasd detach --root /absolute/path/to/repo --revision 3 \
  --fingerprint <fingerprint-from-preview> --apply AGENT.md

# If conservative recovery cannot prove a safe action, inspect the paths and
# archive only the blocking journal. This never touches source/target content.
ewasd recover --discard <journal-id> --confirm

# Empty registrations can be removed explicitly. This refuses any managed or
# unowned source content.
ewasd unregister --project <project-id> --revision <current> --confirm
```

### Protected Git clean

`ewasd clean` keeps the useful `git clean` workflow while protecting every
managed path with exact `-e` patterns, including shared worktree and retained
legacy-residual blocks. It cleans the detected project's scope only and never
passes arbitrary arguments through to Git.

```bash
# Preview ordinary untracked files (default; no writes).
ewasd clean

# Equivalent explicit preview.
ewasd clean --dry-run

# Apply exactly the reviewed plan.
ewasd clean --apply --revision <revision> --fingerprint <fingerprint>

# Include ignored files and untracked directories, like the old `-fdx` flow.
ewasd clean --mode all --directories

# Remove ignored files only (managed entries remain protected).
ewasd clean --mode ignored
```

Git receives only one `-f`; nested Git repositories are skipped by Git's own
safety rule.
The operation is re-planned under the ewasd process lock immediately before
execution, previously healthy managed links are verified afterward, and the
exact candidate list is retained in `clean-records/` and Activity. Defaults are
preview-only, ordinary untracked files, and files-only; `--mode all
--directories` is the explicit equivalent of the old broad cleanup.

In `--mode ignored`, Git's negation rules may retain additional ignored junk
inside a parent directory that also contains a managed entry. This is an
intentional safe-direction tradeoff: ewasd never deletes the parent merely to
clean unrelated ignored children.

All commands support `--json`. Mutations without `--apply` return a complete
plan. A stale `--revision` is rejected rather than silently applying an old
decision to new state.

The web API is stricter still: preview returns a random, one-use `plan_id` that
expires after ten minutes. Apply must present that ID with the identical
project, action, path, and revision. A failed or replayed apply requires a fresh
preview.

## Web console

Loopback mode generates a persistent random token on first start and prints a
one-time pairing URL:

```bash
ewasd serve
# open the printed http://127.0.0.1:7337/?token=... URL
```

For access from another trusted workstation, explicitly bind to the LAN and
allowlist the host/IP that the browser will use. The same generated token
remains mandatory; wildcard binds are rejected without `--allow-host`.

```bash
ewasd serve --listen 0.0.0.0:7337 \
  --allow-host 192.168.1.50 \
  --tls-cert /path/to/lan-cert.pem --tls-key /path/to/lan-key.pem
```

Open the printed pairing URL once. The UI moves the token to
`sessionStorage`, removes it from the address bar, and sends it as a bearer
token. The token is generated at `$EWASD_HOME/console.token` with mode
`0600`; `EWASD_TOKEN` can override it without exposing a secret in `ps`.
The server rejects unapproved Host headers, validates same-origin browser
writes, caps request bodies, and
serves a restrictive content-security policy. For quick testing on an isolated
trusted LAN the TLS flags can be omitted; use TLS or a VPN for routine remote
access. Never expose this filesystem authority directly to the public internet.

## Replacing Python ewasd

```bash
./scripts/replace-python-with-go.sh
```

The script builds and activates the Go package first, then previews and applies
`migrate-legacy` against generated `.ewasd_gitignore` inventories, verifies
every exact link, and archives the marker files and a migration receipt. The Nix
profile is replaced without a command-absence window. Upgradeable elements use `profile upgrade`;
otherwise Go ewasd is added at higher priority and verified before the hidden
Python element is removed. The Python workspace is retained as a
read-only rollback source; the live links no longer point to it.

The script never gives the entire working tree to a Nix `path:` flake. It first
creates an allowlisted installer source containing only `flake.*`, `go.mod`,
`cmd/`, and `internal/` under the private Go state. This prevents ignored or
untracked project files from being copied into the world-readable Nix store.

The migration command can also be run directly:

```bash
ewasd migrate-legacy --workspace ~/.local/share/ewasd \
  --scan-root ~/git
ewasd migrate-legacy --workspace ~/.local/share/ewasd \
  --scan-root ~/git --apply
```

## Current scope

Supported:

- Linux/macOS-style regular files and directories;
- explicit Git checkout registration, including a checkout rooted in a
  monorepo subdirectory;
- adoption, verification, reconciliation, detach, activity history, and crash
  recovery;
- CLI JSON output and a responsive web console.

Deliberately omitted:

- automatic path-only project creation without explicit confirmation;
- deleting source content;
- overwriting conflicts or adopting foreign symlinks;
- escaping/broken nested symlinks or special files inside adopted directories;
- templates, profiles/inheritance, hooks, package installation, Git push, and
  an MCP surface.

Contained relative nested symlinks are preserved exactly. Other omitted features
can be reconsidered without weakening the storage and recovery invariants.
