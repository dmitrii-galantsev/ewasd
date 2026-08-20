# ewasd

`ewasd` is an explicit, journaled repository-overlay manager. Version 1 is a
complete Go replacement for the former Python implementation.

The Go binary is a plan-first CLI for scripts and recovery.

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
nix build
./result/bin/ewasd --version
```

Or build the Go binary directly:

```bash
go build -o /tmp/ewasd ./cmd/ewasd
```

## Build and test

```bash
go test -race ./...
go vet ./...
go build ./cmd/ewasd
gofmt -l .
```

ewasd has a data root:

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

See [Configuration](#configuration) below for the full resolution order
(including a global `--workspace` flag and a config file), `ewasd config`
for inspecting what was actually resolved, and `ewasd init` for creating or
cloning a data root.

## Configuration

Every subcommand that touches the data root — everything except `completion`,
`help`, and `version` — resolves it in this order, first match wins:

1. `--workspace PATH`, accepted before or after the subcommand
   (`ewasd --workspace /path status` and `ewasd status --workspace /path`
   both work);
2. `EWASD_HOME`;
3. `EWASD_NEXT_HOME` (a transitional alias kept for comparing this Go
   rewrite against the old Python tool side by side);
4. `EWASD_WORKSPACE` — the old Python tool's name for this same override,
   kept for backward compatibility;
5. the `workspace` key in `$XDG_CONFIG_HOME/ewasd/config.toml` (default
   `~/.config/ewasd/config.toml`);
6. `$XDG_DATA_HOME/ewasd-v2` (default `~/.local/share/ewasd-v2`).

The old tool's legacy auto-discovery — guessing at a workspace by checking
whether an `editors.toml` happened to exist at a few hardcoded locations,
including `~/git/editor_workspaces` — is deliberately not restored. That
kind of implicit, ambient-state guessing is exactly what this rewrite's
explicit-registration model exists to remove.

`config.toml` supports two keys, using the old tool's names:

```toml
# ~/.config/ewasd/config.toml
workspace = "/absolute/or/~-relative/path"
remote_keys = ["remote.origin.url", "remote.upstream.url"]
```

- `workspace` is a plain quoted string; a leading `~/` is expanded.
- `remote_keys` is the ordered list of `git config` keys ewasd checks when
  looking for a checkout's identifying remote URL; the first key with a
  non-empty value wins. It defaults to `["remote.origin.url"]` when unset.

There is no TOML library in this dependency-free build, so `config.toml` is
parsed by a small, strict subset parser: comments, blank lines, quoted
strings, and single-line string arrays (trailing comma allowed). Anything
else — table headers, bare ints/bools, multiline arrays — is rejected with
an error naming the file and the offending line, rather than silently
misread. A missing `config.toml` is not an error; a malformed one always is.

Run `ewasd config` to see exactly what was resolved and *why* — the data
root and its source, the config file path and whether it exists, the
remote keys and their source, and the state manifest's path and revision
(if readable). It is read-only: it never creates the data root as a side
effect.

```text
$ ewasd config
data root:   /home/you/.local/share/ewasd-v2 (default)
             exists: yes
config file: /home/you/.config/ewasd/config.toml (not found)
remote keys: [remote.origin.url] (default)
state.json:  /home/you/.local/share/ewasd-v2/state.json (revision 7)
```

`ewasd init [--from-git URL]` creates or bootstraps a data root:

```bash
# Create a fresh, empty data root (idempotent; reports whether it already
# existed).
ewasd init

# Bootstrap a data root on a second machine by cloning an existing one.
ewasd init --from-git git@github.com:you/ewasd-data.git
```

`--from-git` refuses to clone into an existing non-empty data root — move
it aside first rather than risk merging or overwriting it. After cloning,
`init` creates any subdirectories the clone didn't have, locks the root
down to mode `0700`, and validates that a cloned `state.json` actually
parses at the schema version this binary understands, cleaning up the
partial directory on any failure so a retry starts clean.

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

## MCP server

`ewasd mcp` runs a Model Context Protocol server over stdio so an agent can
drive the same engine the CLI does. It is a hand-rolled implementation with
**zero external dependencies** (`go.mod` has none, and `flake.nix` relies on
that with `vendorHash = null`) — newline-delimited JSON-RPC 2.0, one message
per line, standard library only. Nothing but JSON-RPC frames is ever written
to stdout; diagnostics go to stderr.

It follows the CLI's preview-then-apply safety model exactly:

- Read-only tools (`status`, `detect`, and every `plan_*` tool) never mutate
  anything. `status` is the call to make first — it bundles project health,
  the manifest revision, the data root, best-effort detection for the
  current directory, and outstanding recovery journals, so an agent rarely
  needs to chain `detect` + `status` + `recover` separately.
- `register` and `link` are safe to call directly. `link` only ever creates
  symlinks that are currently missing; it never replaces a conflict.
- `adopt`, `detach`, and `reconcile` each require the exact `revision` and
  `fingerprint` returned by a matching `plan_adopt` / `plan_detach` /
  `plan_reconcile` call made immediately before. These are never defaulted,
  auto-filled, or auto-fetched — the whole safety model depends on a
  specific, unchanged plan having been reviewed first. A stale value fails
  the call instead of silently re-approving it.
- Engine-level failures (not found, conflict, ambiguous detection, stale
  revision, missing `revision`/`fingerprint`, ...) come back as ordinary tool
  results — `{"ok": false, "error": "<code>", "message": ..., "hints": [...]}`
  with `isError: false` — so a model can read the reason and self-correct
  instead of blindly retrying. JSON-RPC-level errors and `isError: true` are
  reserved for actual protocol problems: malformed JSON, unknown methods, and
  unknown tool names.

Exposed tools: `status`, `detect`, `plan_link`, `plan_adopt`, `plan_detach`,
`plan_reconcile`, `plan_clean` (all read-only), plus `register`, `link`,
`adopt`, `detach`, `reconcile`.

Deliberately **not** exposed, and why:

- **Applying `clean`.** `git clean` permanently deletes untracked files and
  directories, and unlike adopt/detach/reconcile there is no way to pin the
  exact working-tree state between preview and execution with a fingerprint.
  `plan_clean` still returns the exact plan, including the literal `git`
  command, for a human to review and run themselves.
- **`unregister`.** The CLI gates this behind `--confirm` plus an
  "empty checkout only" rule that is easy for a model to get subtly wrong.
  Removing a project from the manifest should be a deliberate human action.
- **Applying `recover`.** Crash recovery inspects and repairs interrupted,
  partially-applied filesystem transactions. Recovering the wrong way can
  destroy the only remaining copy of real content; it needs a human looking
  at the actual paths on disk. `status` still reports outstanding recovery
  journals, so an agent can always surface that attention is needed.
- **Discarding recovery journals (`recover --discard`).** Same reasoning as
  recover — discarding a journal without inspecting the filesystem first can
  hide data loss instead of preventing it.

Client configuration (stdio, no arguments beyond `mcp`):

```json
{
  "mcpServers": {
    "ewasd": {
      "command": "ewasd",
      "args": ["mcp"]
    }
  }
}
```

## Shell completions

`ewasd completion [bash|fish|zsh] [--install]` prints a completion script for
the requested shell, or installs it to the conventional location for that
shell when `--install` is given. Omit the shell name and it is detected from
`$SHELL`. Every generated script is a thin shell-specific wrapper: the actual
completion decisions (which verb, which flag, which project ID, which
managed file) are all resolved dynamically against live state by a hidden
`ewasd __complete` helper, not hard-coded into the script.

```sh
ewasd completion bash --install   # ~/.local/share/bash-completion/completions/ewasd
ewasd completion zsh  --install   # ~/.local/share/zsh/site-functions/_ewasd (add to $fpath)
ewasd completion fish --install   # ~/.config/fish/completions/ewasd.fish
```

`--project` and `--root` complete registered project IDs, names, and roots;
`ewasd detach <TAB>` completes only the paths ewasd actually manages for the
detected or selected project; `ewasd adopt <TAB>` completes unmanaged files
and directories in that checkout; `--mode` and `--discard` complete from live
clean modes and outstanding recovery journals respectively.

## Current scope

Supported:

- Linux/macOS-style regular files and directories;
- explicit Git checkout registration, including a checkout rooted in a
  monorepo subdirectory;
- adoption, verification, reconciliation, detach, activity history, and crash
  recovery;
- CLI JSON output.

Deliberately omitted:

- automatic path-only project creation without explicit confirmation;
- deleting source content;
- overwriting conflicts or adopting foreign symlinks;
- escaping/broken nested symlinks or special files inside adopted directories;
- templates, profiles/inheritance, hooks, package installation, and Git push.

Contained relative nested symlinks are preserved exactly. Other omitted features
can be reconsidered without weakening the storage and recovery invariants.
