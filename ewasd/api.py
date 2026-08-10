"""Structured, side-effect-reporting API for ewasd.

This layer exists so that non-terminal consumers -- an MCP server, an editor
plugin, another Python program -- can drive ewasd without scraping printed
output or interpreting bare exit codes.

Design rules, all of which the CLI violates by necessity:

* Nothing is printed. Every user-facing message emitted by :mod:`ewasd.core`
  is captured and returned as structured data.
* No implicit ambient state. ``cwd`` and ``workspace`` are always explicit
  arguments, never read from the process.
* Failures are values, not exceptions or exit codes. Every result carries
  ``ok``, an ``error`` code, a human ``detail`` and actionable ``hints``.
* Results are JSON-serialisable via :func:`to_jsonable`.
"""

from __future__ import annotations

import dataclasses
import os
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import core
from .core import (
    GITIGNORE_FILENAME,
    ConfigParser,
    Message,
    Repo,
    build_git_clean_tokens,
    capture_messages,
    check_symlink_health,
    collect_remotes,
    detect_repo_name,
    find_repo_name_in_path,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
)

__all__ = [
    "AddFilesResult",
    "ConfigInfo",
    "DetectResult",
    "DoctorResult",
    "GitCleanResult",
    "InitResult",
    "LinkResult",
    "ListResult",
    "Result",
    "StatusResult",
    "add_files",
    "describe_config",
    "detect",
    "doctor",
    "git_clean_args",
    "init",
    "link",
    "list_configs",
    "status",
    "to_jsonable",
]


# --------------------------------------------------------------------------
# Result types
# --------------------------------------------------------------------------


@dataclass
class Result:
    """Base envelope shared by every API call."""

    ok: bool = True
    error: str | None = None
    """Stable machine-readable error code, e.g. ``"no_workspace"``."""
    detail: str | None = None
    """Human-readable explanation of ``error``."""
    hints: list[str] = field(default_factory=list)
    """Concrete next actions a caller (or agent) can take to recover."""
    messages: list[dict[str, str]] = field(default_factory=list)
    """Captured core output: ``{"level": "warn"|"info"|"success", "text": ...}``."""

    @property
    def warnings(self) -> list[str]:
        return [m["text"] for m in self.messages if m["level"] == "warn"]


@dataclass
class ConfigInfo(Result):
    workspace: str = ""
    workspace_exists: bool = False
    editors_toml: str = ""
    editors_toml_exists: bool = False
    tool_config: str = ""
    tool_config_exists: bool = False
    remote_keys: list[str] = field(default_factory=list)
    known_repos: list[str] = field(default_factory=list)


@dataclass
class DetectResult(Result):
    repo_name: str | None = None
    matched_by: str | None = None
    """Which strategy won: ``"project_override"``, ``"path"`` or ``"remote"``."""
    cwd: str = ""
    git_root: str | None = None
    remotes: list[str] = field(default_factory=list)
    known_repos: list[str] = field(default_factory=list)
    trace: list[str] = field(default_factory=list)
    """Why each detection strategy did or did not match. Populated on success too."""


@dataclass
class ConfigEntry:
    name: str
    kind: str  # "file" | "dir" | "missing"
    source: str
    target: str
    linked: bool


@dataclass
class ListResult(Result):
    repo_name: str | None = None
    link_dir: str | None = None
    entries: list[ConfigEntry] = field(default_factory=list)


@dataclass
class DoctorResult(Result):
    repo_name: str | None = None
    healthy: bool = True
    ok_count: int = 0
    problems: list[dict[str, str]] = field(default_factory=list)
    checked: list[dict[str, str]] = field(default_factory=list)


@dataclass
class LinkResult(Result):
    repo_name: str | None = None
    dry_run: bool = False
    linked: list[str] = field(default_factory=list)
    skipped: list[str] = field(default_factory=list)
    gitignore: str | None = None


@dataclass
class AddFilesResult(Result):
    repo_name: str | None = None
    link_dir: str | None = None
    added: list[str] = field(default_factory=list)
    skipped: list[dict[str, str]] = field(default_factory=list)
    created_repo_entry: bool = False


@dataclass
class GitCleanResult(Result):
    repo_name: str | None = None
    tokens: list[str] = field(default_factory=list)
    command: str = ""


@dataclass
class InitResult(Result):
    workspace: str = ""
    created: bool = False


@dataclass
class StatusResult(Result):
    config: ConfigInfo | None = None
    detection: DetectResult | None = None
    entries: list[ConfigEntry] = field(default_factory=list)
    healthy: bool | None = None
    problems: list[dict[str, str]] = field(default_factory=list)
    summary: str = ""


def to_jsonable(obj: Any) -> Any:
    """Recursively convert dataclasses / paths into JSON-safe primitives."""
    if dataclasses.is_dataclass(obj) and not isinstance(obj, type):
        return {k: to_jsonable(v) for k, v in dataclasses.asdict(obj).items()}
    if isinstance(obj, Path):
        return str(obj)
    if isinstance(obj, dict):
        return {k: to_jsonable(v) for k, v in obj.items()}
    if isinstance(obj, list | tuple):
        return [to_jsonable(v) for v in obj]
    return obj


# --------------------------------------------------------------------------
# Internal helpers
# --------------------------------------------------------------------------


def _dump(messages: list[Message]) -> list[dict[str, str]]:
    return [{"level": m.level, "text": m.text} for m in messages]


def _resolve_cwd(cwd: str | Path | None) -> Path:
    return Path(cwd).expanduser().resolve() if cwd else Path.cwd()


def _git_root(cwd: Path) -> str | None:
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "--show-toplevel"], cwd=str(cwd), text=True, stderr=subprocess.PIPE
        ).strip()
        return out or None
    except (OSError, subprocess.CalledProcessError):
        return None


def _load_config(workspace: str | Path | None) -> tuple[ConfigParser | None, Result]:
    """Load editors.toml, converting every failure mode into a Result."""
    ws = get_workspace_dir(str(workspace) if workspace else None)
    try:
        return ConfigParser(workspace_dir=ws), Result()
    except FileNotFoundError:
        return None, Result(
            ok=False,
            error="no_workspace",
            detail=f"No ewasd workspace found at {ws} (missing editors.toml).",
            hints=[
                "Call the 'init' tool to create a workspace.",
                "Or pass an explicit 'workspace' path if one already exists elsewhere.",
                "Or set the EWASD_WORKSPACE environment variable.",
            ],
        )
    except ValueError as exc:
        return None, Result(
            ok=False,
            error="invalid_editors_toml",
            detail=str(exc),
            hints=[f"Fix the malformed entry in {ws / 'editors.toml'}."],
        )


def _resolve_repo(
    cwd: Path, workspace: str | Path | None, project: str | None
) -> tuple[Repo | None, ConfigParser | None, Result]:
    """Resolve the Repo for *cwd*, or a Result explaining why we could not."""
    cfg, err = _load_config(workspace)
    if cfg is None:
        return None, None, err

    trace: list[str] = []
    with capture_messages():
        name = detect_repo_name(
            project_override=project,
            remotes=collect_remotes(cwd),
            cwd=cwd,
            known_repo_names=cfg.repo_names(),
            trace=trace,
        )
    if not name:
        known = list(cfg.repo_names())
        return (
            None,
            cfg,
            Result(
                ok=False,
                error="repo_not_detected",
                detail=f"Could not determine which configured repo applies to {cwd}.",
                hints=[
                    "Pass 'project' explicitly to override detection.",
                    f"Configured repos: {', '.join(known) if known else '(none)'}",
                    *trace,
                ],
            ),
        )
    try:
        return cfg.get_repo(name), cfg, Result()
    except (KeyError, ValueError) as exc:
        return None, cfg, Result(ok=False, error="repo_not_configured", detail=str(exc))


def _inherit(dst: Result, src: Result) -> None:
    dst.ok = src.ok
    dst.error = src.error
    dst.detail = src.detail
    dst.hints = src.hints
    dst.messages = src.messages


# --------------------------------------------------------------------------
# Read-only operations
# --------------------------------------------------------------------------


def describe_config(workspace: str | Path | None = None) -> ConfigInfo:
    """Report the resolved workspace, config paths and configured repos."""
    ws = get_workspace_dir(str(workspace) if workspace else None)
    editors = ws / "editors.toml"
    tool_cfg = get_config_dir() / "config.toml"

    known: list[str] = []
    with capture_messages() as msgs:
        if editors.exists():
            try:
                known = list(ConfigParser(workspace_dir=ws).repo_names())
            except ValueError:
                known = []

    return ConfigInfo(
        workspace=str(ws),
        workspace_exists=ws.exists(),
        editors_toml=str(editors),
        editors_toml_exists=editors.exists(),
        tool_config=str(tool_cfg),
        tool_config_exists=tool_cfg.exists(),
        remote_keys=get_remote_keys(),
        known_repos=known,
        messages=_dump(msgs),
    )


def detect(
    cwd: str | Path | None = None, workspace: str | Path | None = None, project: str | None = None
) -> DetectResult:
    """Explain which configured repo applies to *cwd*, and why."""
    path = _resolve_cwd(cwd)
    cfg, err = _load_config(workspace)
    if cfg is None:
        res = DetectResult(cwd=str(path))
        _inherit(res, err)
        return res

    trace: list[str] = []
    known = list(cfg.repo_names())
    with capture_messages() as msgs:
        remotes = collect_remotes(path)
        name = detect_repo_name(
            project_override=project, remotes=remotes, cwd=path, known_repo_names=known, trace=trace
        )

    git_root = _git_root(path)
    matched_by = _explain_match(name, project, path, known, git_root, remotes, trace)

    return DetectResult(
        ok=name is not None,
        error=None if name else "repo_not_detected",
        detail=None if name else f"No configured repo matches {path}.",
        hints=[] if name else ["Pass 'project' explicitly, or add the repo to editors.toml."],
        repo_name=name,
        matched_by=matched_by,
        cwd=str(path),
        git_root=git_root,
        remotes=remotes,
        known_repos=known,
        trace=trace,
        messages=_dump(msgs),
    )


def _explain_match(  # noqa: PLR0913 - all inputs are needed to name the winning strategy
    name: str | None,
    project: str | None,
    path: Path,
    known: list[str],
    git_root: str | None,
    remotes: list[str],
    trace: list[str],
) -> str | None:
    """Record which strategy produced *name*, so success is as explainable as failure."""
    if name is None:
        return None
    if project and name == project:
        trace.append(f"matched by explicit project override '{project}'")
        return "project_override"
    root = Path(git_root) if git_root else None
    if find_repo_name_in_path(path, known, root) == name:
        trace.append(
            f"matched by path component '{name}' under git root {git_root or '(none)'}; "
            "path match takes precedence over the git remote"
        )
        return "path"
    trace.append(f"matched by git remote {remotes[0] if remotes else '(none)'} -> '{name}'")
    return "remote"


def list_configs(
    cwd: str | Path | None = None, workspace: str | Path | None = None, project: str | None = None
) -> ListResult:
    """List the config entries ewasd manages for the detected repo."""
    path = _resolve_cwd(cwd)
    with capture_messages() as msgs:
        repo, _cfg, err = _resolve_repo(path, workspace, project)
    if repo is None:
        res = ListResult()
        _inherit(res, err)
        res.messages = _dump(msgs)
        return res

    with capture_messages() as msgs2:
        entries = [_entry(repo, path, name) for name in repo.get_configs()]

    return ListResult(
        repo_name=repo.name,
        link_dir=str(repo.link_dir),
        entries=entries,
        messages=_dump(msgs) + _dump(msgs2),
    )


def _entry(repo: Repo, cwd: Path, name: str) -> ConfigEntry:
    src = repo.link_dir / name
    dst = cwd / name
    kind = "dir" if src.is_dir() else "file" if src.is_file() else "missing"
    linked = dst.is_symlink() and dst.exists() and dst.resolve() == src.resolve()
    return ConfigEntry(name=name, kind=kind, source=str(src), target=str(dst), linked=linked)


def doctor(
    cwd: str | Path | None = None, workspace: str | Path | None = None, project: str | None = None
) -> DoctorResult:
    """Check every expected symlink and report broken or mis-pointed ones."""
    path = _resolve_cwd(cwd)
    with capture_messages() as msgs:
        repo, _cfg, err = _resolve_repo(path, workspace, project)
        if repo is None:
            res = DoctorResult()
            _inherit(res, err)
            res.messages = _dump(msgs)
            return res
        statuses = check_symlink_health(path, repo)

    checked = [{"path": str(s.path), "target": str(s.target), "reason": s.reason} for s in statuses]
    problems = [c for c, s in zip(checked, statuses, strict=True) if not s.ok]

    # check_symlink_health only walks entries that still exist in the workspace, so a
    # link whose source was deleted disappears from the report entirely. Sweep the
    # project directory for dangling links that point into the workspace.
    seen = {c["path"] for c in checked}
    for orphan in _find_orphans(path, repo.link_dir, seen):
        checked.append(orphan)
        problems.append(orphan)
    return DoctorResult(
        repo_name=repo.name,
        healthy=not problems,
        ok_count=len(statuses) - len(problems),
        problems=problems,
        checked=checked,
        messages=_dump(msgs),
        hints=(
            ["Run the 'migrate' workflow or re-link to repair broken symlinks."] if problems else []
        ),
    )


def _find_orphans(cwd: Path, link_dir: Path, seen: set[str]) -> list[dict[str, str]]:
    """Find dangling symlinks under *cwd* that point into the workspace link dir."""
    found: list[dict[str, str]] = []
    link_dir_str = str(link_dir)
    try:
        children = sorted(cwd.iterdir())
    except OSError:
        return found
    for item in children:
        if item.name in core.IGNORED_VCS_DIRS or str(item) in seen:
            continue
        if not item.is_symlink():
            continue
        target = os.readlink(item)
        if link_dir_str in target and not item.exists():
            found.append(
                {
                    "path": str(item),
                    "target": target,
                    "reason": "orphaned",
                    "note": "symlink points into the workspace but the source no longer exists",
                }
            )
    return found


def git_clean_args(
    cwd: str | Path | None = None, workspace: str | Path | None = None, project: str | None = None
) -> GitCleanResult:
    """Return the ``-e <path>`` tokens that protect ewasd links from git clean.

    Read-only on purpose: this hands the caller a command to review and run
    themselves rather than executing a destructive ``git clean``.
    """
    path = _resolve_cwd(cwd)
    with capture_messages() as msgs:
        repo, _cfg, err = _resolve_repo(path, workspace, project)
    if repo is None:
        res = GitCleanResult()
        _inherit(res, err)
        res.messages = _dump(msgs)
        return res
    with capture_messages() as msgs2:
        tokens = build_git_clean_tokens(repo)
    return GitCleanResult(
        repo_name=repo.name,
        tokens=tokens,
        command="git clean -xdf " + " ".join(tokens),
        messages=_dump(msgs) + _dump(msgs2),
    )


def status(
    cwd: str | Path | None = None, workspace: str | Path | None = None, project: str | None = None
) -> StatusResult:
    """One-call orientation: config + detection + entries + link health.

    Intended as the first call a client makes, so it does not have to chain
    four requests to work out what state the directory is in.
    """
    path = _resolve_cwd(cwd)
    cfg_info = describe_config(workspace)
    det = detect(path, workspace, project)

    res = StatusResult(config=cfg_info, detection=det)
    if not det.ok:
        res.ok = False
        res.error = det.error
        res.detail = det.detail
        res.hints = det.hints
        res.summary = f"No ewasd repo resolved for {path}."
        return res

    listing = list_configs(path, workspace, project)
    health = doctor(path, workspace, project)
    res.entries = listing.entries
    res.healthy = health.healthy
    res.problems = health.problems
    linked = sum(1 for e in listing.entries if e.linked)
    res.summary = (
        f"repo '{det.repo_name}': {linked}/{len(listing.entries)} top-level entries linked, "
        f"{len(health.problems)} problem(s)."
    )
    return res


# --------------------------------------------------------------------------
# Mutating operations
# --------------------------------------------------------------------------


def link(
    cwd: str | Path | None = None,
    workspace: str | Path | None = None,
    project: str | None = None,
    dry_run: bool = True,
) -> LinkResult:
    """Create the symlinks for the detected repo.

    Defaults to ``dry_run=True``: a caller must opt in to mutating the disk.
    """
    path = _resolve_cwd(cwd)
    with capture_messages() as msgs:
        repo, _cfg, err = _resolve_repo(path, workspace, project)
        if repo is None:
            res = LinkResult(dry_run=dry_run)
            _inherit(res, err)
            res.messages = _dump(msgs)
            return res
        linked = repo.link_all(path, dry_run=dry_run)

    dumped = _dump(msgs)
    gitignore = path / GITIGNORE_FILENAME
    return LinkResult(
        repo_name=repo.name,
        dry_run=dry_run,
        linked=linked,
        skipped=[m["text"] for m in dumped if m["level"] == "warn"],
        gitignore=str(gitignore) if (not dry_run and gitignore.exists()) else None,
        messages=dumped,
    )


def add_files(
    files: list[str],
    cwd: str | Path | None = None,
    workspace: str | Path | None = None,
    project: str | None = None,
) -> AddFilesResult:
    """Move local file(s) into the workspace and symlink them back."""
    path = _resolve_cwd(cwd)
    if not files:
        return AddFilesResult(
            ok=False, error="no_files", detail="Provide at least one file path to add."
        )

    cfg, err = _load_config(workspace)
    if cfg is None:
        res = AddFilesResult()
        _inherit(res, err)
        return res

    with capture_messages() as msgs:
        outcome = core.add_files(files, path, cfg, project)

    return AddFilesResult(
        ok=outcome.exit_code == 0,
        error=None if outcome.exit_code == 0 else "add_failed",
        detail=None if outcome.exit_code == 0 else "One or more files could not be added.",
        repo_name=outcome.repo_name,
        link_dir=str(outcome.link_dir) if outcome.link_dir else None,
        added=outcome.added,
        skipped=outcome.skipped,
        created_repo_entry=outcome.created_repo_entry,
        messages=_dump(msgs),
    )


def init(workspace: str | Path | None = None, from_git: str | None = None) -> InitResult:
    """Create (or clone) an ewasd workspace."""
    ws = get_workspace_dir(str(workspace) if workspace else None)
    existed = (ws / "editors.toml").exists()
    with capture_messages() as msgs:
        try:
            code = core.init_workspace(ws, from_git)
        except (OSError, subprocess.CalledProcessError) as exc:
            return InitResult(
                ok=False,
                error="init_failed",
                detail=str(exc),
                workspace=str(ws),
                messages=_dump(msgs),
            )
    return InitResult(
        ok=code == 0,
        error=None if code == 0 else "init_failed",
        workspace=str(ws),
        created=not existed and (ws / "editors.toml").exists(),
        messages=_dump(msgs),
    )
