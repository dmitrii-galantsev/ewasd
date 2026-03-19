"""Command line interface for ewasd."""

import argparse
import subprocess
import sys
from importlib.metadata import PackageNotFoundError, version
from pathlib import Path

from .completions import generate_bash_completion, generate_fish_completion, generate_zsh_completion
from .core import (
    ConfigParser,
    add_file_to_repo,
    build_git_clean_tokens,
    check_symlink_health,
    collect_remotes,
    detect_repo_name,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
    init_workspace,
    migrate_symlinks,
    warn,
)


def get_version() -> str:
    """Get the package version from installed metadata."""
    try:
        return version("ewasd")
    except PackageNotFoundError:
        return "unknown (not installed)"


def install_completion(shell: str, content: str) -> bool:
    """Install completion script to the appropriate location."""
    locations = {
        "bash": [
            Path.home() / ".bash_completion.d" / "ewasd",
            Path.home() / ".local/share/bash-completion/completions/ewasd",
        ],
        "fish": [Path.home() / ".config/fish/completions/ewasd.fish"],
        "zsh": [
            Path.home() / ".zsh/completions/_ewasd",
            Path.home() / ".local/share/zsh/site-functions/_ewasd",
        ],
    }

    for path in locations.get(shell, []):
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
            print(f"Installed {shell} completion to: {path}")
            if shell == "zsh":
                print("Note: Make sure the completion directory is in your $fpath")
            return True
        except (OSError, PermissionError):
            continue

    warn(f"Could not install {shell} completion automatically")
    print(f"Please save this content to an appropriate {shell} completion location:")
    print(content)
    return False


def handle_completion(shell: str, install: bool) -> int:
    """Handle completion generation and optional installation."""
    generators = {
        "bash": generate_bash_completion,
        "fish": generate_fish_completion,
        "zsh": generate_zsh_completion,
    }

    if shell not in generators:
        warn(f"Unsupported shell: {shell}")
        return 1

    content = generators[shell]()

    if install:
        return 0 if install_completion(shell, content) else 1
    else:
        print(content)
        return 0


def handle_init(workspace: str | None, from_git: str | None) -> int:
    """Initialize a new ewasd workspace."""
    ws = get_workspace_dir(workspace)
    return init_workspace(ws, from_git)


def handle_migrate(
    workspace: str | None, old_workspace: str | None, scan_dir: str | None, dry_run: bool
) -> int:
    """Fix broken symlinks by repointing them from old workspace to new workspace."""
    ws = get_workspace_dir(workspace)

    if not old_workspace:
        warn("--old-workspace is required for migrate")
        warn("Usage: ewasd migrate --old-workspace /path/to/old/workspace")
        return 1

    old_ws = Path(old_workspace).expanduser().resolve()

    if not ws.exists():
        warn(f"New workspace {ws} does not exist. Run 'ewasd init' first.")
        return 1

    target_dir = Path(scan_dir).resolve() if scan_dir else Path.cwd()

    print(f"Scanning {target_dir} for symlinks pointing to {old_ws}")
    print(f"Will repoint to {ws}")
    if dry_run:
        print("(dry run - no changes will be made)\n")
    else:
        print()

    fixed, broken = migrate_symlinks(ws, old_ws, target_dir, dry_run)

    if dry_run:
        print(f"\nDry run complete. Would fix {fixed} symlink(s).")
    else:
        print(f"\nMigration complete. Fixed {fixed} symlink(s).")
    if broken:
        print(f"  {broken} symlink(s) could not be fixed (target missing in new workspace)")
    return 0


def handle_config(workspace: str | None) -> int:
    """Show resolved configuration paths and settings."""
    ws = get_workspace_dir(workspace)
    config_dir = get_config_dir()
    config_file = config_dir / "config.toml"
    toml_path = ws / "editors.toml"
    remote_keys = get_remote_keys()

    config_status = "found" if config_file.exists() else "not found"
    print(f"workspace:   {ws}")
    print(f"config:      {config_file} ({config_status})")
    print(f"editors:     {toml_path}")
    print(f"remote_keys: {remote_keys}")
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="ewasd",
        description=(
            "Symlink curated editor / tooling configuration files (dotfiles, etc.) "
            "into the current working directory based on matching git remote URL."
        ),
    )
    parser.add_argument("-V", "--version", action="version", version=f"%(prog)s {get_version()}")
    parser.add_argument(
        "--workspace",
        type=str,
        default=None,
        help="Path to ewasd workspace directory (overrides all other workspace resolution)",
    )
    parser.add_argument(
        "--project",
        type=str,
        help="Explicitly specify the project name to link configs for (monorepo support)",
    )
    parser.add_argument(
        "--add-file",
        type=str,
        nargs="+",
        help="Move specified file(s) to central repo and create symlink back (auto-creates project entry if needed)",
    )
    sub = parser.add_subparsers(dest="command", required=False)

    link_p = sub.add_parser("link", help="Link all discovered config entries (default action)")
    link_p.add_argument(
        "-n",
        "--dry-run",
        action="store_true",
        help="Show what symlinks would be created without actually creating them",
    )
    sub.add_parser("list", help="List config entries for the detected repo")
    sub.add_parser("version", help="Show version information")
    sub.add_parser(
        "git-clean-args", help="Emit -e <path> tokens suitable for passing to git clean -fdx"
    )
    clean_p = sub.add_parser(
        "clean", help="Run git clean while preserving linked configs (wrapper around git clean)"
    )
    clean_p.add_argument(
        "--dry-run",
        "-n",
        action="store_true",
        help="Show what would be removed (passes -n to git clean)",
    )
    clean_p.add_argument(
        "--directories",
        "-d",
        action="store_true",
        help="Include directories (adds -d, default: include because original examples used -fdx)",
    )
    clean_p.add_argument(
        "--force",
        "-f",
        action="store_true",
        help="Force (adds -f). If not supplied we still add one -f for safety parity with examples.",
    )
    clean_p.add_argument(
        "--extra",
        action="append",
        default=[],
        help="Extra arguments to pass through to git clean (can be repeated)",
    )

    # Init command
    init_p = sub.add_parser("init", help="Initialize a new ewasd workspace")
    init_p.add_argument(
        "--from-git", type=str, default=None, help="Clone workspace from a git repository URL"
    )

    # Config command
    sub.add_parser("config", help="Show resolved configuration paths and settings")

    # Doctor command
    sub.add_parser("doctor", help="Check symlink health and report broken/stale links")

    # Migrate command
    migrate_p = sub.add_parser("migrate", help="Fix broken symlinks after workspace relocation")
    migrate_p.add_argument(
        "--old-workspace",
        type=str,
        required=True,
        help="Path to the old workspace directory that symlinks currently point to",
    )
    migrate_p.add_argument(
        "--scan-dir",
        type=str,
        default=None,
        help="Directory to scan for broken symlinks (default: cwd)",
    )
    migrate_p.add_argument(
        "-n",
        "--dry-run",
        action="store_true",
        help="Show what would be fixed without making changes",
    )

    # Completion generation
    completion_p = sub.add_parser("completion", help="Generate shell completion scripts")
    completion_p.add_argument(
        "shell", choices=["bash", "fish", "zsh"], help="Shell to generate completion for"
    )
    completion_p.add_argument(
        "--install", action="store_true", help="Install completion script to standard location"
    )

    return parser.parse_args(argv)


def resolve_command(ns: argparse.Namespace) -> str:
    return ns.command or "link"


def handle_add_file(
    filenames: list[str], cwd: Path, cfg: ConfigParser, project_override: str | None
) -> int:
    """Handle --add-file: move file(s) to central repo and create symlink back."""
    return add_file_to_repo(filenames, cwd, cfg, project_override)


def main(argv: list[str] | None = None) -> int:
    if argv is None:
        argv = sys.argv[1:]
    ns = parse_args(argv)
    command = resolve_command(ns)

    workspace = getattr(ns, "workspace", None)

    # Handle commands that don't need repo detection early
    if command == "completion":
        return handle_completion(ns.shell, ns.install)
    if command == "version":
        print(f"ewasd {get_version()}")
        return 0
    if command == "init":
        return handle_init(workspace, getattr(ns, "from_git", None))
    if command == "config":
        return handle_config(workspace)
    if command == "migrate":
        return handle_migrate(
            workspace,
            getattr(ns, "old_workspace", None),
            getattr(ns, "scan_dir", None),
            getattr(ns, "dry_run", False),
        )

    try:
        cfg = ConfigParser(workspace_dir=get_workspace_dir(workspace) if workspace else None)
    except FileNotFoundError:
        ws = get_workspace_dir(workspace)
        warn(f"No workspace found at {ws}")
        warn("Run 'ewasd init' to create a new workspace, or")
        warn("Run 'ewasd init --from-git <url>' to clone an existing one, or")
        warn("Set EWASD_WORKSPACE to point to your workspace directory")
        return 1
    cwd = Path.cwd()

    # Handle --add-file first (before repo resolution)
    if getattr(ns, "add_file", None):
        return handle_add_file(ns.add_file, cwd, cfg, getattr(ns, "project", None))

    # Monorepo support: if --project is given, use it directly
    trace: list[str] = []
    if getattr(ns, "project", None):
        repo_name = detect_repo_name(
            project_override=ns.project,
            remotes=collect_remotes(),
            cwd=cwd,
            known_repo_names=cfg.repo_names(),
            trace=trace,
        )
    else:
        repo_name = detect_repo_name(
            project_override=None,
            remotes=collect_remotes(),
            cwd=cwd,
            known_repo_names=cfg.repo_names(),
            trace=trace,
        )
    if not repo_name:
        warn("Unable to determine repository name. Strategies tried:")
        for t in trace:
            warn(f"  - {t}")
        return 1

    try:
        repo = cfg.get_repo(repo_name)
    except (KeyError, ValueError) as exc:
        warn(str(exc))
        return 1

    # cwd already captured above

    if command == "doctor":
        results = check_symlink_health(cwd, repo)
        if not results:
            print("No symlinks found to check.")
            return 0
        broken = [r for r in results if not r.ok]
        ok_count = len(results) - len(broken)
        for r in results:
            if r.ok:
                print(f"  OK: {r.path}")
            elif r.reason == "broken":
                print(f"  BROKEN: {r.path} -> {r.target} (target missing)")
            elif r.reason == "wrong_target":
                print(f"  WRONG:  {r.path} -> {r.target} (expected to point into workspace)")
        print(f"\n{ok_count} ok, {len(broken)} problem(s)")
        return 1 if broken else 0
    if command == "list":
        for c in repo.get_configs():
            print(c)
        return 0
    if command == "git-clean-args":
        tokens = build_git_clean_tokens(repo)
        print(" ".join(tokens))
        return 0
    if command == "link":
        dry_run = getattr(ns, "dry_run", False)
        if dry_run:
            print("Dry run - showing planned operations:")
        else:
            print("Attempting link:")
        repo.link_all(cwd, dry_run=dry_run)
        if dry_run:
            print("Dry run complete. No changes made.")
        else:
            print("Done!")
        return 0
    if command == "clean":
        # Build base args replicating prior usage: git clean -fdx plus exclusions
        exclusions = build_git_clean_tokens(repo)
        ns_flags = ["git", "clean"]
        # Always include -x to remove ignored, and -f once. Add -d if requested.
        ns_flags.append("-x")
        if ns.directories:
            ns_flags.append("-d")
        # Add at least one -f (git requires it); add a second if user specified --force
        ns_flags.append("-f")
        if ns.force:
            ns_flags.append("-f")
        if ns.dry_run:
            ns_flags.append("-n")
        # Insert exclusions
        ns_flags.extend(exclusions)
        # Any extra raw args
        ns_flags.extend(ns.extra)
        print("Executing:", " ".join(ns_flags))
        try:
            subprocess.run(ns_flags, check=True)
        except subprocess.CalledProcessError as e:
            warn(f"git clean failed: {e}")
            return e.returncode
        return 0
    warn(f"Unknown command: {command}")
    return 2


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
