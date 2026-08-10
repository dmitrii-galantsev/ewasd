"""MCP server exposing ewasd over the Model Context Protocol.

Run with ``ewasd-mcp`` (stdio transport). Install with ``pip install 'ewasd[mcp]'``.

Design notes
------------
* Every tool takes an explicit ``cwd``; agents frequently operate outside the
  server's own working directory, and guessing is how config lands in the
  wrong repository.
* Mutating tools are separate from read tools, are annotated with
  ``readOnlyHint`` / ``destructiveHint`` / ``idempotentHint``, and ``link``
  defaults to ``dry_run=True`` so a plan is always available before any write.
* ``git clean`` is deliberately *not* executed. ``git_clean_args`` returns the
  command for a human to run; a tool that silently deletes untracked files is
  not something an agent should be able to call.
* Errors are returned as data (``ok: false`` plus ``hints``) instead of raised,
  so a model can self-correct rather than retry blindly.
* Nothing prints: core output is captured and returned in ``messages``,
  because stdout is the JSON-RPC transport.

Supports both the 1.x (``FastMCP``) and 2.x (``MCPServer``) Python SDKs.
"""

from __future__ import annotations

import importlib
import json
import os
from typing import Any

from mcp.types import ToolAnnotations

from . import api

# The SDK renamed FastMCP -> MCPServer in 2.0. Resolve at runtime so a single
# codebase works against both; the two classes are API-compatible for our use.
_SERVER_CANDIDATES = (("mcp.server", "MCPServer"), ("mcp.server.fastmcp", "FastMCP"))


def _server_class() -> Any:
    for module_name, attr in _SERVER_CANDIDATES:
        try:
            return getattr(importlib.import_module(module_name), attr)
        except (ImportError, AttributeError):
            continue
    raise ImportError(
        "No compatible MCP server class found. Install with: pip install 'ewasd[mcp]'"
    )


INSTRUCTIONS = """\
ewasd symlinks curated editor/tooling config files from a central workspace into git
repositories, keeping the configs out of the repository itself.

Start with `status`: it reports the resolved workspace, which configured repo matches the
directory, which entries are linked, and any broken symlinks. Prefer it over chaining
describe_config + detect + list_configs + doctor.

If a repo cannot be detected the result carries `ok: false`, an `error` code, and `hints`
plus a per-strategy `trace` explaining what was tried. The usual fix is to pass `project`
explicitly (required inside monorepos), not to retry.

Always pass an absolute `cwd`. Always call `link` with dry_run=true and show the plan to
the user before calling it again with dry_run=false, unless the user has already approved
in advance. Never invent a workspace path; read it from `describe_config`.

`add_files` MOVES files into the workspace and replaces them with symlinks. Say so before
using it. Everything else only ever adds symlinks and never overwrites an existing file.
"""

mcp = _server_class()("ewasd", instructions=INSTRUCTIONS)


def _annotations(*, read_only: bool, destructive: bool = False, idempotent: bool = True) -> Any:
    """Build ToolAnnotations using the wire-format aliases (works on 1.x and 2.x)."""
    return ToolAnnotations.model_validate(
        {
            "readOnlyHint": read_only,
            "destructiveHint": destructive,
            "idempotentHint": idempotent,
            "openWorldHint": False,
        }
    )


READ = _annotations(read_only=True)
WRITE = _annotations(read_only=False, destructive=False, idempotent=True)

# `add_files` MOVES a user file into the workspace and leaves a symlink behind.
# Content is never lost and the original path still resolves, so by default it
# is advertised as non-destructive -- clients that auto-deny destructive tools
# (Codex with approval_policy="never", for example) would otherwise refuse it
# outright. Set EWASD_MCP_STRICT=1 to advertise it as destructive and force
# those clients to ask a human first.
STRICT = os.environ.get("EWASD_MCP_STRICT", "").lower() in {"1", "true", "yes"}
MOVE = _annotations(read_only=False, destructive=STRICT, idempotent=False)


def _json(payload: Any) -> str:
    return json.dumps(api.to_jsonable(payload), indent=2, sort_keys=False)


# --------------------------------------------------------------------------
# Read-only tools
# --------------------------------------------------------------------------


@mcp.tool(title="Workspace status", annotations=READ)
def status(
    cwd: str | None = None, workspace: str | None = None, project: str | None = None
) -> api.StatusResult:
    """Orientation call: resolved config, detected repo, entries and link health.

    Prefer this over chaining describe_config + detect + list_configs + doctor.

    Args:
        cwd: Absolute path of the project directory to inspect. Defaults to the
            server's working directory, which is usually not what you want.
        workspace: Override the ewasd workspace location.
        project: Override repo auto-detection (needed inside monorepos).
    """
    return api.status(cwd, workspace, project)


@mcp.tool(title="Show ewasd configuration", annotations=READ)
def describe_config(workspace: str | None = None) -> api.ConfigInfo:
    """Show resolved ewasd paths: workspace, editors.toml, tool config, known repos.

    Use this to discover valid `project` names and the real workspace path
    instead of guessing either.
    """
    return api.describe_config(workspace)


@mcp.tool(title="Detect project", annotations=READ)
def detect(
    cwd: str | None = None, workspace: str | None = None, project: str | None = None
) -> api.DetectResult:
    """Explain which configured repo maps to a directory, and why.

    Returns `trace`, a per-strategy log (path-based match, then git remote),
    which is the fastest way to diagnose "unable to determine repository name".
    """
    return api.detect(cwd, workspace, project)


@mcp.tool(title="List managed configs", annotations=READ)
def list_configs(
    cwd: str | None = None, workspace: str | None = None, project: str | None = None
) -> api.ListResult:
    """List config entries ewasd manages for a directory, each with linked state.

    Only top-level entries are listed; a directory entry may itself contain
    files that are linked individually.
    """
    return api.list_configs(cwd, workspace, project)


@mcp.tool(title="Check symlink health", annotations=READ)
def doctor(
    cwd: str | None = None, workspace: str | None = None, project: str | None = None
) -> api.DoctorResult:
    """Check symlink health: reports broken links and links pointing elsewhere.

    Run this after moving a workspace or after a git operation that may have
    clobbered links.
    """
    return api.doctor(cwd, workspace, project)


@mcp.tool(title="Build safe git clean command", annotations=READ)
def git_clean_args(
    cwd: str | None = None, workspace: str | None = None, project: str | None = None
) -> api.GitCleanResult:
    """Build a `git clean` command that preserves ewasd-managed symlinks.

    Returns the command as text for a human to review and run. This tool does
    not execute it -- deleting untracked files is not reversible.
    """
    return api.git_clean_args(cwd, workspace, project)


# --------------------------------------------------------------------------
# Mutating tools
# --------------------------------------------------------------------------


@mcp.tool(title="Link configs into project", annotations=WRITE)
def link(
    cwd: str | None = None,
    workspace: str | None = None,
    project: str | None = None,
    dry_run: bool = True,
) -> api.LinkResult:
    """Symlink the workspace's config files into a project directory.

    Call with dry_run=true first, present the plan, then call again with
    dry_run=false once the user approves. Existing non-symlink files are never
    overwritten -- they appear in `skipped`.

    Args:
        cwd: Absolute path of the project directory to link into.
        workspace: Override the ewasd workspace location.
        project: Override repo auto-detection.
        dry_run: When true (default) nothing is written to disk.
    """
    return api.link(cwd, workspace, project, dry_run=dry_run)


@mcp.tool(title="Move files into workspace", annotations=MOVE)
def add_files(
    files: list[str],
    cwd: str | None = None,
    workspace: str | None = None,
    project: str | None = None,
) -> api.AddFilesResult:
    """Move local file(s) into the central workspace and symlink them back.

    This MOVES the files -- the originals are replaced by symlinks. Paths are
    relative to `cwd`. A repo entry is auto-created in editors.toml if none
    exists. Files already present in the workspace are reported in `skipped`
    with a reason rather than being overwritten.
    """
    return api.add_files(files, cwd, workspace, project)


@mcp.tool(title="Initialize workspace", annotations=WRITE)
def init(workspace: str | None = None, from_git: str | None = None) -> api.InitResult:
    """Create a new ewasd workspace, optionally cloning one from a git URL.

    Safe to call when a workspace already exists; an existing editors.toml is
    not overwritten.
    """
    return api.init(workspace, from_git)


# --------------------------------------------------------------------------
# Resources
# --------------------------------------------------------------------------


@mcp.resource("ewasd://config")
def config_resource() -> str:
    """Current resolved ewasd configuration and the list of configured repos."""
    return _json(api.describe_config(None))


def main() -> None:
    """Console-script entry point: serve over stdio."""
    mcp.run()


if __name__ == "__main__":  # pragma: no cover
    main()
