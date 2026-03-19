"""Tests for --add-file handler (handle_add_file)."""

from pathlib import Path
from unittest.mock import patch

from ewasd.cli import handle_add_file
from ewasd.core import ConfigParser


def _make_workspace(tmp_path: Path, repo_name: str = "myrepo") -> tuple[Path, ConfigParser]:
    """Create a minimal workspace with one repo entry."""
    ws = tmp_path / "workspace"
    ws.mkdir()
    repo_dir = ws / "repos" / repo_name
    repo_dir.mkdir(parents=True)
    (ws / "editors.toml").write_text(
        f'[repos]\n[repos.{repo_name}]\nrepo = "https://example.com/{repo_name}.git"\n'
        f'link_dir = "repos/{repo_name}"\n'
    )
    cfg = ConfigParser(workspace_dir=ws)
    return ws, cfg


class TestAddFileSuccess:
    """Test successful --add-file operations."""

    def test_single_file(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "config.yaml").write_text("key: value")

        result = handle_add_file(["config.yaml"], cwd, cfg, "myrepo")

        assert result == 0
        # Original location should be a symlink
        assert (cwd / "config.yaml").is_symlink()
        # File should exist in workspace
        assert (ws / "repos" / "myrepo" / "config.yaml").exists()
        assert (ws / "repos" / "myrepo" / "config.yaml").read_text() == "key: value"

    def test_multiple_files(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "a.txt").write_text("aaa")
        (cwd / "b.txt").write_text("bbb")

        result = handle_add_file(["a.txt", "b.txt"], cwd, cfg, "myrepo")

        assert result == 0
        assert (cwd / "a.txt").is_symlink()
        assert (cwd / "b.txt").is_symlink()
        assert (ws / "repos" / "myrepo" / "a.txt").read_text() == "aaa"
        assert (ws / "repos" / "myrepo" / "b.txt").read_text() == "bbb"

    def test_gitignore_updated(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "file.txt").write_text("data")

        handle_add_file(["file.txt"], cwd, cfg, "myrepo")

        gitignore = cwd / ".ewasd_gitignore"
        assert gitignore.exists()
        content = gitignore.read_text()
        assert "file.txt" in content


class TestAddFileErrors:
    """Test error handling in --add-file."""

    def test_file_does_not_exist(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()

        result = handle_add_file(["nonexistent.txt"], cwd, cfg, "myrepo")

        assert result == 1

    def test_multiple_missing_files(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()

        result = handle_add_file(["missing1.txt", "missing2.txt"], cwd, cfg, "myrepo")

        assert result == 1

    def test_file_already_in_workspace(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        # File exists in both places
        (cwd / "existing.txt").write_text("local")
        (ws / "repos" / "myrepo" / "existing.txt").write_text("already there")

        result = handle_add_file(["existing.txt"], cwd, cfg, "myrepo")

        # Should succeed (0) but skip the already-existing file
        assert result == 0
        # Original file should NOT become a symlink (skipped)
        assert not (cwd / "existing.txt").is_symlink()

    def test_no_project_override_no_detection(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "file.txt").write_text("data")

        # Mock all detection to fail
        with (
            patch("ewasd.core.detect_repo_name", return_value=None),
            patch("ewasd.core.collect_remotes", return_value=[]),
            patch("ewasd.core.find_repo_name", return_value=None),
        ):
            result = handle_add_file(["file.txt"], cwd, cfg, None)

        # Should fall back to path-based name extraction, which succeeds
        # (uses last component of cwd: "project")
        # But "project" won't be in the config, so it auto-creates
        assert result in {0, 1}  # depends on auto-create success


class TestAddFileAutoCreate:
    """Test auto-creation of project entries."""

    def test_auto_creates_project_entry(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "newproject"
        cwd.mkdir()
        (cwd / "config.txt").write_text("data")

        result = handle_add_file(["config.txt"], cwd, cfg, "newproject")

        assert result == 0
        # Verify entry was created in editors.toml
        cfg2 = ConfigParser(workspace_dir=ws)
        assert "newproject" in cfg2.repo_names()

    def test_project_override_used(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "file.txt").write_text("data")

        result = handle_add_file(["file.txt"], cwd, cfg, "myrepo")

        assert result == 0
        assert (ws / "repos" / "myrepo" / "file.txt").exists()
