"""Tests for --rm-file handler."""

from pathlib import Path
from unittest.mock import patch

from ewasd.core import ConfigParser, add_file_to_repo, rm_file_from_repo


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


def _fake_trash(removed: list[Path]):
    """Return a _trash_path replacement that records and unlinks the target."""

    def _impl(path: Path) -> bool:
        removed.append(path)
        path.unlink()
        return True

    return _impl


class TestRmFileSuccess:
    """Test successful --rm-file operations (inverse of --add-file)."""

    def test_single_file(self, tmp_path: Path):
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "config.yaml").write_text("key: value")
        add_file_to_repo(["config.yaml"], cwd, cfg, "myrepo")
        central = ws / "repos" / "myrepo" / "config.yaml"
        assert (cwd / "config.yaml").is_symlink()
        assert central.exists()

        removed: list[Path] = []
        with patch("ewasd.core._trash_path", _fake_trash(removed)):
            result = rm_file_from_repo(["config.yaml"], cwd, cfg, "myrepo")

        assert result == 0
        # Local symlink gone, central copy trashed.
        assert not (cwd / "config.yaml").exists()
        assert not (cwd / "config.yaml").is_symlink()
        assert central.resolve() in [p.resolve() for p in removed]
        assert not central.exists()

    def test_multiple_files(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "a.txt").write_text("aaa")
        (cwd / "b.txt").write_text("bbb")
        add_file_to_repo(["a.txt", "b.txt"], cwd, cfg, "myrepo")

        removed: list[Path] = []
        with patch("ewasd.core._trash_path", _fake_trash(removed)):
            result = rm_file_from_repo(["a.txt", "b.txt"], cwd, cfg, "myrepo")

        assert result == 0
        assert not (cwd / "a.txt").is_symlink()
        assert not (cwd / "b.txt").is_symlink()
        assert len(removed) == 2

    def test_gitignore_entry_dropped(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "keep.txt").write_text("keep")
        (cwd / "drop.txt").write_text("drop")
        add_file_to_repo(["keep.txt", "drop.txt"], cwd, cfg, "myrepo")

        with patch("ewasd.core._trash_path", _fake_trash([])):
            rm_file_from_repo(["drop.txt"], cwd, cfg, "myrepo")

        content = (cwd / ".ewasd_gitignore").read_text()
        assert "keep.txt" in content
        assert "drop.txt" not in content

    def test_remove_last_file_empties_gitignore(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "only.txt").write_text("data")
        add_file_to_repo(["only.txt"], cwd, cfg, "myrepo")

        with patch("ewasd.core._trash_path", _fake_trash([])):
            result = rm_file_from_repo(["only.txt"], cwd, cfg, "myrepo")

        assert result == 0
        content = (cwd / ".ewasd_gitignore").read_text()
        assert "only.txt" not in content


class TestRmFileErrors:
    """Test error handling in --rm-file."""

    def test_file_does_not_exist(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()

        with patch("ewasd.core._trash_path", _fake_trash([])):
            result = rm_file_from_repo(["nonexistent.txt"], cwd, cfg, "myrepo")

        assert result == 1

    def test_regular_file_not_managed(self, tmp_path: Path):
        """A plain (non-symlink) file is not touched."""
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "plain.txt").write_text("not managed")

        removed: list[Path] = []
        with patch("ewasd.core._trash_path", _fake_trash(removed)):
            result = rm_file_from_repo(["plain.txt"], cwd, cfg, "myrepo")

        assert result == 1
        # Untouched: still a regular file, nothing trashed.
        assert (cwd / "plain.txt").is_file()
        assert not (cwd / "plain.txt").is_symlink()
        assert removed == []

    def test_symlink_to_unrelated_target(self, tmp_path: Path):
        """A symlink that does not point into the central repo is left alone."""
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        outside = tmp_path / "outside.txt"
        outside.write_text("elsewhere")
        (cwd / "link.txt").symlink_to(outside)

        removed: list[Path] = []
        with patch("ewasd.core._trash_path", _fake_trash(removed)):
            result = rm_file_from_repo(["link.txt"], cwd, cfg, "myrepo")

        assert result == 1
        assert (cwd / "link.txt").is_symlink()
        assert removed == []

    def test_project_not_found(self, tmp_path: Path):
        _ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()

        result = rm_file_from_repo(["file.txt"], cwd, cfg, "doesnotexist")

        assert result == 1

    def test_broken_symlink_into_repo(self, tmp_path: Path):
        """If the central copy is already gone, still remove the dangling symlink."""
        ws, cfg = _make_workspace(tmp_path)
        cwd = tmp_path / "project"
        cwd.mkdir()
        (cwd / "f.txt").write_text("data")
        add_file_to_repo(["f.txt"], cwd, cfg, "myrepo")
        # Simulate the central file vanishing (symlink now broken).
        (ws / "repos" / "myrepo" / "f.txt").unlink()

        removed: list[Path] = []
        with patch("ewasd.core._trash_path", _fake_trash(removed)):
            result = rm_file_from_repo(["f.txt"], cwd, cfg, "myrepo")

        assert result == 0
        assert not (cwd / "f.txt").is_symlink()
        assert removed == []
