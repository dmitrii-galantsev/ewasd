"""ewasd package.

Provides functionality to link curated editor / tooling configuration files from a central
repository into individual project working directories.
"""

from .core import (
    GITIGNORE_FILENAME,
    IGNORED_VCS_DIRS,
    ConfigParser,
    Repo,
    add_file_to_repo,
    collect_remotes,
    find_repo_name,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
    init_workspace,
    migrate_symlinks,
)

__all__ = [
    "GITIGNORE_FILENAME",
    "IGNORED_VCS_DIRS",
    "ConfigParser",
    "Repo",
    "add_file_to_repo",
    "collect_remotes",
    "find_repo_name",
    "get_config_dir",
    "get_remote_keys",
    "get_workspace_dir",
    "init_workspace",
    "migrate_symlinks",
]
