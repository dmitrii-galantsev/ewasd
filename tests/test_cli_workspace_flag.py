"""Tests for --workspace CLI flag and init/config commands."""

import tomllib
from pathlib import Path

from ewasd.cli import handle_config, handle_init, parse_args
from ewasd.core import ConfigParser


class TestWorkspaceFlag:
    """Verify --workspace flag is parsed and passed through."""

    def test_workspace_flag_passed_to_config_parser(self, tmp_path: Path):
        """--workspace flag reaches parse_args with correct value."""
        ns = parse_args(["--workspace", str(tmp_path), "list"])
        assert ns.workspace == str(tmp_path)

    def test_workspace_flag_with_link(self, tmp_path: Path):
        """ewasd --workspace /path link uses the specified workspace."""
        ws = tmp_path / "ws"
        ws.mkdir()
        (ws / "repos" / "myrepo").mkdir(parents=True)
        (ws / "repos" / "myrepo" / ".clangd").write_text("---")
        (ws / "editors.toml").write_text(
            "[repos]\n"
            "[repos.myrepo]\n"
            'repo = "https://example.com/myrepo.git"\n'
            'link_dir = "repos/myrepo"\n'
        )

        cfg = ConfigParser(workspace_dir=ws)
        repo = cfg.get_repo("myrepo")
        assert repo.link_dir == ws / "repos" / "myrepo"


class TestInit:
    """Verify `ewasd init` creates correct workspace structure."""

    def test_init_creates_structure(self, tmp_path: Path):
        """init creates workspace dir, repos/, and starter editors.toml."""
        ws = tmp_path / "new-workspace"
        handle_init(workspace=str(ws), from_git=None)

        assert ws.is_dir()
        assert (ws / "repos").is_dir()
        assert (ws / "editors.toml").exists()

        # Starter toml should be valid and have [repos] table
        with (ws / "editors.toml").open("rb") as f:
            data = tomllib.load(f)
        assert "repos" in data

    def test_init_does_not_overwrite_existing_toml(self, tmp_path: Path):
        """init preserves existing editors.toml."""
        ws = tmp_path / "existing"
        ws.mkdir()
        (ws / "editors.toml").write_text('[repos]\n[repos.mine]\nrepo = "x"\nlink_dir = "y"\n')

        handle_init(workspace=str(ws), from_git=None)

        text = (ws / "editors.toml").read_text()
        assert "mine" in text  # original content preserved

    def test_init_idempotent(self, tmp_path: Path):
        """Running init twice is safe."""
        ws = tmp_path / "ws"
        handle_init(workspace=str(ws), from_git=None)
        handle_init(workspace=str(ws), from_git=None)
        assert (ws / "editors.toml").exists()
        assert (ws / "repos").is_dir()


class TestConfig:
    """Verify `ewasd config` shows resolved paths."""

    def test_config_shows_paths(self, tmp_path: Path, capsys, monkeypatch):
        """config command prints workspace and config paths."""
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
        result = handle_config(workspace=str(tmp_path))

        assert result == 0
        output = capsys.readouterr().out
        assert str(tmp_path) in output
        assert "workspace:" in output
        assert "config:" in output
        assert "remote_keys:" in output
