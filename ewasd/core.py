"""Core domain objects and helpers for ewasd.

Separable from CLI so it can be imported and unit tested.
"""

import os
import re
import subprocess
import sys
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path

try:  # Optional color support
    from termcolor import colored  # type: ignore
except Exception:  # pragma: no cover - optional dependency

    def colored(text: str, _color: str) -> str:  # type: ignore
        return text


import tomllib  # Python 3.11+ built-in for reading TOML


def get_config_dir() -> Path:
    """Return XDG_CONFIG_HOME/ewasd, creating nothing."""
    base = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    return base / "ewasd"


def _read_tool_config() -> dict:  # type: ignore[type-arg]
    """Load ~/.config/ewasd/config.toml if it exists."""
    config_file = get_config_dir() / "config.toml"
    if config_file.exists():
        with config_file.open("rb") as f:
            return tomllib.load(f)
    return {}


def get_workspace_dir(cli_override: str | None = None) -> Path:
    """Resolve workspace directory in priority order.

    1. CLI flag (--workspace)
    2. EWASD_WORKSPACE env var
    3. workspace key in ~/.config/ewasd/config.toml
    4. $XDG_DATA_HOME/ewasd/ (if it exists)
    5. Legacy fallback (backwards compat during transition)
    6. Default to XDG data home even if it doesn't exist yet
    """
    # 1. CLI flag
    if cli_override:
        return Path(cli_override).expanduser().resolve()
    # 2. Env var
    env = os.environ.get("EWASD_WORKSPACE")
    if env:
        return Path(env).expanduser().resolve()
    # 3. config.toml
    tool_cfg = _read_tool_config()
    if "workspace" in tool_cfg:
        return Path(tool_cfg["workspace"]).expanduser().resolve()
    # 4. XDG data home
    data_home = Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local" / "share"))
    xdg_path = data_home / "ewasd"
    if xdg_path.exists():
        return xdg_path
    # 5. Legacy fallback (backwards compat during transition)
    legacy = Path(__file__).resolve().parent.parent
    if (legacy / "editors.toml").exists():
        return legacy
    legacy2 = Path.home() / "git" / "editor_workspaces"
    if (legacy2 / "editors.toml").exists():
        return legacy2
    # 6. Default to XDG even if it doesn't exist yet
    return xdg_path


def get_remote_keys() -> list[str]:
    """Load remote keys from config, default to just origin."""
    cfg = _read_tool_config()
    return cfg.get("remote_keys", ["remote.origin.url"])


def warn(msg: str) -> None:
    print(colored(f"WARN: {msg}", "yellow"), file=sys.stderr)


def success(msg: str) -> None:
    print(colored(msg, "green"))


@dataclass
class Repo:
    """A configured repository entry from editors.toml."""

    name: str
    git_url: str
    link_dir: Path

    def get_configs(self) -> list[str]:
        """Return candidate config entries (files or directories) at top level.

        Currently returns everything in link_dir (non-recursive). A future enhancement
        might allow filtering (e.g., only dotfiles) via a predicate parameter.
        """
        if not self.link_dir.exists():
            warn(f"Directory {self.link_dir} does not exist")
            return []
        entries: list[str] = []
        for child in sorted(self.link_dir.iterdir()):
            # Skip version control internals just in case
            if child.name in {".git", ".svn"}:
                continue
            entries.append(child.name)
        return entries

    def link_directory(self, src_dir: Path, dst_dir: Path, rel_prefix: str = "") -> list[str]:
        """Recursively link files from src_dir into dst_dir.

        Returns list of relative paths that were symlinked.
        """
        linked_paths: list[str] = []

        if not src_dir.exists() or not src_dir.is_dir():
            return linked_paths

        for child in sorted(src_dir.iterdir()):
            if child.name in {".git", ".svn"}:
                continue

            src_item = src_dir / child.name
            dst_item = dst_dir / child.name
            rel_path = f"{rel_prefix}{child.name}"

            if src_item.is_dir():
                if dst_item.exists() and dst_item.is_dir():
                    # Both are directories, recurse
                    nested_paths = self.link_directory(src_item, dst_item, f"{rel_path}/")
                    linked_paths.extend(nested_paths)
                elif not dst_item.exists():
                    # Target dir doesn't exist, symlink the whole directory
                    success(f"Linked {src_item} to {dst_item}")
                    dst_item.symlink_to(src_item)
                    linked_paths.append(rel_path)
                # If dst_item exists but is a file, skip with warning
                elif dst_item.is_file():
                    warn(f"File exists where directory expected! Skipping {rel_path}")
            # It's a file
            elif dst_item.is_symlink():
                # Already a symlink, track it
                linked_paths.append(rel_path)
            elif dst_item.exists():
                # Real file exists, skip
                warn(f"File exists! Skipping {rel_path}")
            else:
                # Link the file
                success(f"Linked {src_item} to {dst_item}")
                dst_item.symlink_to(src_item)
                linked_paths.append(rel_path)

        return linked_paths

    def link_any(self, target_name: str, cwd: Path) -> list[str]:
        """Link a single config entry (file or directory).

        Returns list of relative paths that were symlinked.
        """
        src = self.link_dir / target_name
        dst = cwd / target_name

        if not src.exists():
            warn(f"No file found at {src}")
            return []

        # Handle directories
        if src.is_dir():
            if dst.is_symlink():
                warn(f"Symbolic link exists! Skipping {target_name}")
                return [target_name]
            elif dst.exists() and dst.is_dir():
                # Both are directories - recurse into it
                return self.link_directory(src, dst, f"{target_name}/")
            elif dst.exists():
                warn(f"File exists where directory expected! Skipping {target_name}")
                return []
            else:
                # Target dir doesn't exist, symlink whole directory
                success(f"Linked {src} to {dst}")
                dst.symlink_to(src)
                return [target_name]

        # Handle files
        if dst.is_symlink():
            warn(f"Symbolic link exists! Skipping {target_name}")
            return [target_name]
        if dst.exists():
            warn(f"File exists! Skipping {target_name}")
            return []
        success(f"Linked {src} to {dst}")
        dst.symlink_to(src)
        return [target_name]

    def update_gitignore(self, cwd: Path, linked_items: list[str]) -> None:
        """Create/update .ewasd_gitignore with only actually symlinked items.

        For monorepos: consolidates all .ewasd_gitignore files into a single
        file at the git root so that all projects' symlinks are properly ignored.
        """
        if not linked_items:
            return

        # Detect git root for monorepo support
        git_root: Path | None = None
        path_prefix = ""
        # Resolve symlinks in cwd for accurate path comparison
        resolved_cwd = cwd.resolve()
        try:
            out = subprocess.check_output(
                ["git", "rev-parse", "--show-toplevel"], cwd=str(cwd), text=True
            ).strip()
            if out:
                git_root = Path(out).resolve()
                # Calculate relative path from git root to cwd (using resolved paths)
                if resolved_cwd != git_root:
                    rel = resolved_cwd.relative_to(git_root)
                    path_prefix = str(rel) + "/"
        except Exception:
            pass

        # Write local .ewasd_gitignore with this project's entries
        local_gitignore = cwd / ".ewasd_gitignore"
        local_content = "# Auto-generated by ewasd\n"
        local_content += "# Editor configuration files\n"
        local_content += f"{path_prefix}.ewasd_gitignore\n"
        for item in linked_items:
            local_content += f"{path_prefix}{item}\n"
        local_gitignore.write_text(local_content)

        # For monorepos: consolidate all .ewasd_gitignore files into one at git root
        if git_root and git_root != resolved_cwd:
            consolidated_file = git_root / ".ewasd_gitignore"
            all_entries: set[str] = set()

            # Find all .ewasd_gitignore files in the repo
            try:
                for gitignore_path in git_root.rglob(".ewasd_gitignore"):
                    try:
                        content = gitignore_path.read_text()
                        for line in content.splitlines():
                            line = line.strip()
                            # Skip comments and empty lines
                            if line and not line.startswith("#"):
                                all_entries.add(line)
                    except Exception:
                        pass
            except Exception:
                pass

            # Also include the root gitignore file itself
            all_entries.add(".ewasd_gitignore")

            # Write consolidated file at git root
            consolidated_content = "# Auto-generated by ewasd\n"
            consolidated_content += "# Consolidated editor configuration files from all projects\n"
            for entry in sorted(all_entries):
                consolidated_content += f"{entry}\n"
            consolidated_file.write_text(consolidated_content)

            # Point git to the consolidated file at root
            target_gitignore = consolidated_file
        else:
            target_gitignore = local_gitignore

        # Configure git to use this file as an additional excludes file
        try:
            absolute_gitignore = str(target_gitignore.resolve())
            subprocess.run(
                ["git", "config", "core.excludesFile", absolute_gitignore],
                cwd=str(cwd),
                check=True,
                capture_output=True,
            )
        except Exception as e:
            warn(f"Failed to configure git excludes file: {e}")

    def link_all(self, cwd: Path) -> None:
        linked_items = []
        for cfg in self.get_configs():
            paths = self.link_any(cfg, cwd)
            linked_items.extend(paths)
        # Auto-update gitignore with only successfully linked items
        self.update_gitignore(cwd, linked_items)

    def iter_git_clean_args(self) -> Iterable[str]:
        # Produces tokens suitable for: git clean -fdx $(ewasd git-clean-args)
        # Exclude the gitignore file itself
        yield "-e"
        yield ".ewasd_gitignore"
        # Exclude all config files
        for cfg in self.get_configs():
            yield "-e"
            yield cfg


class ConfigParser:
    """Load and provide access to editors.toml configuration."""

    def __init__(self, workspace_dir: Path | None = None, toml_path: Path | None = None):
        if toml_path:
            self.toml_path = toml_path
            self.workspace_dir = toml_path.parent
        else:
            self.workspace_dir = workspace_dir or get_workspace_dir()
            self.toml_path = self.workspace_dir / "editors.toml"
        with self.toml_path.open("rb") as f:
            self.parsed = tomllib.load(f)
        if "repos" not in self.parsed:
            raise ValueError("Invalid editors.toml: missing [repos] table")

    def repo_names(self) -> Sequence[str]:
        return list(self.parsed["repos"].keys())

    def get_repo(self, name: str) -> Repo:
        data = self.parsed["repos"].get(name)
        if not data:
            raise KeyError(f"Repo [{name}] not found in editors.toml")
        git_url = data.get("repo")
        link_dir_raw = data.get("link_dir")
        if not git_url or not link_dir_raw:
            raise ValueError(f"Repo [{name}] missing required fields")
        link_dir = self.workspace_dir / link_dir_raw
        return Repo(name=name, git_url=git_url, link_dir=link_dir)

    def create_repo_entry(self, name: str, cwd: Path) -> Repo | None:
        """Auto-create a new repo entry by inferring from current context."""
        # Try to get git remote for URL
        try:
            remotes = collect_remotes()
            git_url = remotes[0] if remotes else f"https://example.com/{name}.git"
        except Exception:  # pragma: no cover
            git_url = f"https://example.com/{name}.git"

        # Determine link_dir based on existing patterns or default
        # Look for existing patterns to infer vendor/category
        existing_entries = list(self.parsed["repos"].values())
        link_dir_raw = f"repos/{name}"  # default

        if existing_entries:
            # Try to infer pattern from similar entries
            for entry in existing_entries:
                existing_link = entry.get("link_dir", "")
                if "/" in existing_link:
                    parts = existing_link.split("/")
                    if len(parts) >= 2:
                        # Use same vendor/category structure
                        vendor_path = "/".join(parts[:-1])
                        link_dir_raw = f"{vendor_path}/{name}"
                        break

        # Write new entry to TOML file
        try:
            # Read current TOML as text to preserve formatting
            toml_content = self.toml_path.read_text()
            new_entry = f'\n    [{f"repos.{name}"}]\n        repo = "{git_url}"\n        link_dir = "{link_dir_raw}"\n'

            # Append before any trailing content
            if toml_content.endswith("\n"):
                updated_content = toml_content + new_entry
            else:
                updated_content = toml_content + new_entry

            # Write back
            self.toml_path.write_text(updated_content)

            # Reload parsed data
            with self.toml_path.open("rb") as f:
                self.parsed = tomllib.load(f)

            # Return the new repo object
            link_dir = self.workspace_dir / link_dir_raw
            return Repo(name=name, git_url=git_url, link_dir=link_dir)

        except Exception as e:  # pragma: no cover
            warn(f"Failed to write to {self.toml_path}: {e}")
            return None


def collect_remotes() -> list[str]:
    out: list[str] = []
    for key in get_remote_keys():
        try:
            result = subprocess.run(
                ["git", "config", "--get", key], capture_output=True, text=True, check=False
            )
            url = result.stdout.strip()
            if url:
                out.append(url)
        except Exception:
            pass
    return out


RE_REPO_NAME = re.compile(r".*/([^/]+?)(?:\.git|/?|)$")


def find_repo_name(remotes: Sequence[str]) -> str | None:
    for url in remotes:
        m = RE_REPO_NAME.match(url)
        if m:
            return m.group(1)
    return None


def build_git_clean_tokens(repo: Repo) -> list[str]:
    return list(repo.iter_git_clean_args())


def find_repo_name_in_path(
    cwd: Path, known_repo_names: Sequence[str], git_root: Path | None = None
) -> str | None:
    """Attempt to detect a repo name by examining path components.

    This enables monorepo support: if the git remote refers to a monorepo (e.g. 'rocm-systems')
    but you're working inside a subdirectory whose leaf component matches a configured repo
    (e.g. 'projects/rdc'), we use that leaf.
    Preference: choose the *deepest* path component that matches a known repo for specificity.
    """
    # Establish search path relative to git root if provided, else full path.
    parts = list(cwd.parts)
    if git_root and cwd.is_relative_to(git_root):
        rel = cwd.relative_to(git_root)
        parts = [p for p in rel.parts]
    matches = [p for p in parts if p in known_repo_names]
    if matches:
        return matches[-1]
    return None


def detect_repo_name(
    *,
    project_override: str | None,
    remotes: Sequence[str],
    cwd: Path,
    known_repo_names: Sequence[str],
) -> str | None:
    """Unified repo name detection with override, path-first for monorepo, then remote fallback."""
    if project_override:
        return project_override if project_override in known_repo_names else None

    # Path-based detection first (monorepo subdir case)
    # Try to locate git root; if fails, just use full path components.
    git_root: Path | None = None
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "--show-toplevel"], cwd=str(cwd), text=True
        ).strip()
        if out:
            git_root = Path(out)
    except Exception:  # pragma: no cover - non-critical
        pass
    path_match = find_repo_name_in_path(cwd, known_repo_names, git_root)
    if path_match:
        return path_match

    # Remote fallback
    remote_match = find_repo_name(remotes)
    if remote_match and remote_match in known_repo_names:
        return remote_match
    return None
