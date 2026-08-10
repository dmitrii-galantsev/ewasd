"""ewasd package.

Provides functionality to link curated editor / tooling configuration files from a central
repository into individual project working directories.
"""

from . import api
from .core import (
    GITIGNORE_FILENAME,
    IGNORED_VCS_DIRS,
    ConfigParser,
    Message,
    Repo,
    add_file_to_repo,
    add_files,
    capture_messages,
    collect_remotes,
    find_repo_name,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
    init_workspace,
    migrate_symlinks,
    rm_file_from_repo,
)

__all__ = [
    "GITIGNORE_FILENAME",
    "IGNORED_VCS_DIRS",
    "ConfigParser",
    "Message",
    "Repo",
    "add_file_to_repo",
    "add_files",
    "api",
    "capture_messages",
    "collect_remotes",
    "find_repo_name",
    "get_config_dir",
    "get_remote_keys",
    "get_workspace_dir",
    "init_workspace",
    "migrate_symlinks",
    "rm_file_from_repo",
]
