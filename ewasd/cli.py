"""Command line interface for ewasd."""

import argparse
import os
import shutil
import subprocess
import sys
from importlib.metadata import PackageNotFoundError, version
from pathlib import Path

from .completions import generate_bash_completion, generate_fish_completion, generate_zsh_completion
from .core import (
    ConfigParser,
    build_git_clean_tokens,
    collect_remotes,
    detect_repo_name,
    find_repo_name,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
    success,
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


STARTER_TOML = """\
# ewasd workspace configuration
# Add repos here. Each entry maps a git repo to a directory of config files.
#
# [repos.my-project]
#     repo = "https://github.com/user/my-project.git"
#     link_dir = "repos/my-project"
[repos]
"""


def _find_legacy_workspace() -> Path | None:
    """Check common legacy locations for an existing workspace."""
    import __main__  # noqa: PLC0415

    candidates = [
        Path(__main__.__file__).resolve().parent if hasattr(__main__, "__file__") else None,
        Path(__file__).resolve().parent.parent,
        Path.home() / "git" / "editor_workspaces",
        Path.home() / "git" / "ewasd",
    ]
    for candidate in candidates:
        if candidate and (candidate / "editors.toml").exists():
            return candidate
    return None


def handle_init(workspace: str | None, from_git: str | None) -> int:
    """Initialize a new ewasd workspace."""
    ws = get_workspace_dir(workspace)
    if from_git:
        subprocess.run(["git", "clone", from_git, str(ws)], check=True)
        return 0

    # Check for legacy workspace to migrate from
    legacy = _find_legacy_workspace()
    if legacy and legacy.resolve() != ws.resolve() and not (ws / "editors.toml").exists():
        print(f"Found existing workspace at {legacy}")
        print(f"Copying to {ws} ...")
        ws.mkdir(parents=True, exist_ok=True)
        # Copy editors.toml
        shutil.copy2(legacy / "editors.toml", ws / "editors.toml")
        # Copy repos/ directory
        legacy_repos = legacy / "repos"
        if legacy_repos.is_dir():
            dest_repos = ws / "repos"
            if dest_repos.exists():
                shutil.rmtree(dest_repos)
            shutil.copytree(legacy_repos, dest_repos, symlinks=True)
        success(f"Migrated workspace from {legacy} to {ws}")
        print("Run 'ewasd migrate --old-workspace " + str(legacy) + "' to fix existing symlinks.")
        return 0

    ws.mkdir(parents=True, exist_ok=True)
    (ws / "repos").mkdir(exist_ok=True)
    toml = ws / "editors.toml"
    if not toml.exists():
        toml.write_text(STARTER_TOML)
    success(f"Initialized workspace at {ws}")
    return 0


def handle_migrate(
    workspace: str | None, old_workspace: str | None, scan_dir: str | None, dry_run: bool
) -> int:
    """Fix broken symlinks by repointing them from old workspace to new workspace.

    Recursively scans a directory tree for symlinks whose targets contain the old
    workspace path and updates them to point to the equivalent path in the new workspace.
    """
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
    old_ws_str = str(old_ws)
    new_ws_str = str(ws)

    print(f"Scanning {target_dir} for symlinks pointing to {old_ws}")
    print(f"Will repoint to {ws}")
    if dry_run:
        print("(dry run - no changes will be made)\n")
    else:
        print()

    fixed = 0
    broken = 0

    for item in target_dir.rglob("*"):
        if not item.is_symlink():
            continue

        try:
            link_target = os.readlink(item)
        except OSError:
            continue

        if old_ws_str not in link_target:
            continue

        new_target = link_target.replace(old_ws_str, new_ws_str, 1)

        if Path(new_target).exists():
            if dry_run:
                print(f"  Would fix: {item} -> {new_target}")
            else:
                item.unlink()
                item.symlink_to(new_target)
                success(f"  Fixed: {item} -> {new_target}")
            fixed += 1
        else:
            warn(f"  Cannot fix: {item} -> {link_target} (new target {new_target} not found)")
            broken += 1

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

    sub.add_parser("link", help="Link all discovered config entries (default action)")
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
    # Validate all files exist first
    missing_files = []
    for filename in filenames:
        file_path = cwd / filename
        if not file_path.exists():
            missing_files.append(filename)

    if missing_files:
        for filename in missing_files:
            warn(f"File {filename} does not exist in current directory")
        return 1

    # Determine repo name - be more permissive for --add-file
    if project_override:
        repo_name = project_override
    else:
        # Try normal detection first
        repo_name = detect_repo_name(
            project_override=None,
            remotes=collect_remotes(),
            cwd=cwd,
            known_repo_names=cfg.repo_names(),
        )

        # If that fails, try to extract from remote or path without known_repo_names constraint
        if not repo_name:
            # Try remote URL first (priority for --add-file)
            remotes = collect_remotes()
            if remotes:
                repo_name = find_repo_name(remotes)

            # If still no luck, extract from path components (last non-empty component)
            if not repo_name:
                path_parts = [p for p in cwd.parts if p and p != "/"]
                if path_parts:
                    repo_name = path_parts[-1]

    if not repo_name:
        warn("Unable to determine repository name for --add-file operation")
        warn("Please use --project <name> to specify explicitly")
        return 1

    # Ensure project exists in config
    try:
        repo = cfg.get_repo(repo_name)
    except KeyError:
        # Auto-create project entry
        print(f"Project '{repo_name}' not found in config. Creating...")
        repo = cfg.create_repo_entry(repo_name, cwd)
        if not repo:
            warn(f"Failed to create project entry for '{repo_name}'")
            return 1
        success(f"Created project entry: {repo_name} -> {repo.link_dir}")
    except Exception as exc:
        warn(str(exc))
        return 1

    # Ensure target directory exists
    repo.link_dir.mkdir(parents=True, exist_ok=True)

    # Process each file
    successfully_added = []

    for filename in filenames:
        file_path = cwd / filename
        target_path = repo.link_dir / filename

        if target_path.exists():
            warn(f"File {filename} already exists in central repo at {target_path}")
            continue

        # Ensure parent directories exist for the target
        target_path.parent.mkdir(parents=True, exist_ok=True)

        print(f"Moving {file_path} -> {target_path}")
        shutil.move(str(file_path), str(target_path))

        # Create symlink back
        print(f"Creating symlink {file_path} -> {target_path}")
        file_path.symlink_to(target_path)

        successfully_added.append(filename)
        success(f"Successfully added {filename} to central repo and created symlink")

    # Update git configuration to ignore the newly added symlinks
    if successfully_added:
        repo.update_gitignore(cwd, successfully_added)

    return 0


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
    if getattr(ns, "project", None):
        repo_name = ns.project
    else:
        repo_name = detect_repo_name(
            project_override=None,
            remotes=collect_remotes(),
            cwd=cwd,
            known_repo_names=cfg.repo_names(),
        )
    if not repo_name:
        warn("Unable to determine repository name (remote + path + override all failed)")
        return 1

    try:
        repo = cfg.get_repo(repo_name)
    except Exception as exc:
        warn(str(exc))
        return 1

    # cwd already captured above

    if command == "list":
        for c in repo.get_configs():
            print(c)
        return 0
    if command == "git-clean-args":
        tokens = build_git_clean_tokens(repo)
        print(" ".join(tokens))
        return 0
    if command == "link":
        print("Attempting link:")
        repo.link_all(cwd)
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
