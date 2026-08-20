"""Tests for ewasd migrate command."""

import os
from pathlib import Path

from ewasd.cli import handle_migrate


class TestMigrate:
    """Verify migrate fixes broken symlinks after workspace relocation."""

    def test_migrate_fixes_symlinks(self, tmp_path: Path):
        """Symlinks pointing to old workspace are repointed to new workspace."""
        # Set up old and new workspace with identical file
        old_ws = tmp_path / "old-workspace"
        new_ws = tmp_path / "new-workspace"
        (old_ws / "repos" / "myrepo").mkdir(parents=True)
        (new_ws / "repos" / "myrepo").mkdir(parents=True)
        (old_ws / "repos" / "myrepo" / ".clangd").write_text("---")
        (new_ws / "repos" / "myrepo" / ".clangd").write_text("---")
        (new_ws / "editors.toml").write_text("[repos]\n")

        # Create a symlink in a "target repo" pointing to old workspace
        target_repo = tmp_path / "target-repo"
        target_repo.mkdir()
        (target_repo / ".clangd").symlink_to(old_ws / "repos" / "myrepo" / ".clangd")

        # Verify symlink currently points to old location
        assert str(old_ws) in os.readlink(target_repo / ".clangd")

        result = handle_migrate(
            workspace=str(new_ws),
            old_workspace=str(old_ws),
            scan_dir=str(target_repo),
            dry_run=False,
        )

        assert result == 0
        # Verify symlink now points to new location
        new_target = os.readlink(target_repo / ".clangd")
        assert str(new_ws) in new_target
        assert (target_repo / ".clangd").read_text() == "---"

    def test_migrate_dry_run(self, tmp_path: Path):
        """Dry run reports what would be fixed without making changes."""
        old_ws = tmp_path / "old-workspace"
        new_ws = tmp_path / "new-workspace"
        (old_ws / "repos" / "myrepo").mkdir(parents=True)
        (new_ws / "repos" / "myrepo").mkdir(parents=True)
        (old_ws / "repos" / "myrepo" / ".clangd").write_text("---")
        (new_ws / "repos" / "myrepo" / ".clangd").write_text("---")
        (new_ws / "editors.toml").write_text("[repos]\n")

        target_repo = tmp_path / "target-repo"
        target_repo.mkdir()
        (target_repo / ".clangd").symlink_to(old_ws / "repos" / "myrepo" / ".clangd")

        result = handle_migrate(
            workspace=str(new_ws),
            old_workspace=str(old_ws),
            scan_dir=str(target_repo),
            dry_run=True,
        )

        assert result == 0
        # Symlink should still point to old location (dry run)
        assert str(old_ws) in os.readlink(target_repo / ".clangd")

    def test_migrate_recursive(self, tmp_path: Path):
        """Migrate scans subdirectories recursively."""
        old_ws = tmp_path / "old"
        new_ws = tmp_path / "new"
        (old_ws / "repos" / "proj").mkdir(parents=True)
        (new_ws / "repos" / "proj").mkdir(parents=True)
        (old_ws / "repos" / "proj" / "file.txt").write_text("content")
        (new_ws / "repos" / "proj" / "file.txt").write_text("content")
        (new_ws / "editors.toml").write_text("[repos]\n")

        target = tmp_path / "repo" / "subdir"
        target.mkdir(parents=True)
        (target / "file.txt").symlink_to(old_ws / "repos" / "proj" / "file.txt")

        result = handle_migrate(
            workspace=str(new_ws),
            old_workspace=str(old_ws),
            scan_dir=str(tmp_path / "repo"),
            dry_run=False,
        )

        assert result == 0
        assert str(new_ws) in os.readlink(target / "file.txt")

    def test_migrate_requires_old_workspace(self, tmp_path: Path):
        """Migrate fails if --old-workspace not provided."""
        result = handle_migrate(
            workspace=str(tmp_path), old_workspace=None, scan_dir=None, dry_run=False
        )
        assert result == 1

    def test_migrate_skips_non_matching_symlinks(self, tmp_path: Path):
        """Symlinks not pointing to old workspace are left alone."""
        old_ws = tmp_path / "old"
        new_ws = tmp_path / "new"
        new_ws.mkdir()
        (new_ws / "editors.toml").write_text("[repos]\n")

        target = tmp_path / "repo"
        target.mkdir()
        other_target = tmp_path / "other" / "file.txt"
        other_target.parent.mkdir(parents=True)
        other_target.write_text("other")
        (target / "link").symlink_to(other_target)

        result = handle_migrate(
            workspace=str(new_ws), old_workspace=str(old_ws), scan_dir=str(target), dry_run=False
        )

        assert result == 0
        # Should still point to original non-workspace target
        assert os.readlink(target / "link") == str(other_target)
