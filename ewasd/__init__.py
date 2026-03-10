"""ewasd package.

Provides functionality to link curated editor / tooling configuration files from a central
repository into individual project working directories.
"""

from .core import (
    ConfigParser,
    Repo,
    collect_remotes,
    find_repo_name,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
)

__all__ = [
    "ConfigParser",
    "Repo",
    "collect_remotes",
    "find_repo_name",
    "get_config_dir",
    "get_remote_keys",
    "get_workspace_dir",
]
