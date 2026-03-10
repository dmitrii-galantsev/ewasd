"""Tests for XDG-compliant workspace and config resolution."""

from pathlib import Path

import ewasd.core
from ewasd.core import (
    ConfigParser,
    _read_tool_config,
    get_config_dir,
    get_remote_keys,
    get_workspace_dir,
)


class TestGetWorkspaceDir:
    """Verify workspace resolution priority: CLI > env > config.toml > XDG > legacy."""

    def test_cli_override_wins(self, tmp_path: Path):
        """--workspace flag takes highest priority."""
        ws = tmp_path / "my-workspace"
        ws.mkdir()
        result = get_workspace_dir(cli_override=str(ws))
        assert result == ws.resolve()

    def test_cli_override_expands_tilde(self, monkeypatch, tmp_path: Path):
        """CLI override expands ~ to home directory."""
        monkeypatch.setenv("HOME", str(tmp_path))
        result = get_workspace_dir(cli_override="~/my-ws")
        assert result == (tmp_path / "my-ws").resolve()

    def test_env_var_second_priority(self, monkeypatch, tmp_path: Path):
        """EWASD_WORKSPACE env var used when no CLI override."""
        ws = tmp_path / "env-workspace"
        ws.mkdir()
        monkeypatch.setenv("EWASD_WORKSPACE", str(ws))
        result = get_workspace_dir(cli_override=None)
        assert result == ws.resolve()

    def test_config_toml_third_priority(self, monkeypatch, tmp_path: Path):
        """~/.config/ewasd/config.toml workspace key used when no CLI or env."""
        # Set up config dir
        config_dir = tmp_path / "config" / "ewasd"
        config_dir.mkdir(parents=True)
        ws = tmp_path / "toml-workspace"
        ws.mkdir()
        (config_dir / "config.toml").write_text(f'workspace = "{ws}"\n')

        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
        monkeypatch.delenv("EWASD_WORKSPACE", raising=False)
        # Prevent XDG_DATA_HOME from matching
        monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "nonexistent"))

        result = get_workspace_dir(cli_override=None)
        assert result == ws.resolve()

    def test_xdg_data_home_fourth_priority(self, monkeypatch, tmp_path: Path):
        """$XDG_DATA_HOME/ewasd used when it exists and no higher-priority source."""
        data_dir = tmp_path / "share" / "ewasd"
        data_dir.mkdir(parents=True)
        (data_dir / "editors.toml").write_text("[repos]\n")

        monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
        monkeypatch.delenv("EWASD_WORKSPACE", raising=False)
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "no-config"))

        result = get_workspace_dir(cli_override=None)
        assert result == data_dir

    def test_default_xdg_data_home(self, monkeypatch, tmp_path: Path):
        """When XDG_DATA_HOME is unset, defaults to ~/.local/share/ewasd."""
        monkeypatch.setenv("HOME", str(tmp_path))
        monkeypatch.delenv("EWASD_WORKSPACE", raising=False)
        monkeypatch.delenv("XDG_DATA_HOME", raising=False)
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "no-config"))

        # Mock __file__ so legacy fallback doesn't find editors.toml
        monkeypatch.setattr(ewasd.core, "__file__", str(tmp_path / "fake" / "ewasd" / "core.py"))

        result = get_workspace_dir(cli_override=None)
        assert result == tmp_path / ".local" / "share" / "ewasd"

    def test_env_var_overrides_config_toml(self, monkeypatch, tmp_path: Path):
        """Env var has higher priority than config.toml."""
        # Set up config.toml pointing one place
        config_dir = tmp_path / "config" / "ewasd"
        config_dir.mkdir(parents=True)
        (config_dir / "config.toml").write_text(f'workspace = "{tmp_path / "toml-ws"}"\n')
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))

        # Env var points somewhere else
        env_ws = tmp_path / "env-ws"
        env_ws.mkdir()
        monkeypatch.setenv("EWASD_WORKSPACE", str(env_ws))

        result = get_workspace_dir(cli_override=None)
        assert result == env_ws.resolve()


class TestGetConfigDir:
    """Verify config directory follows XDG_CONFIG_HOME."""

    def test_default_config_dir(self, monkeypatch, tmp_path: Path):
        """Default config dir is ~/.config/ewasd."""
        monkeypatch.setenv("HOME", str(tmp_path))
        monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
        assert get_config_dir() == tmp_path / ".config" / "ewasd"

    def test_xdg_config_home_respected(self, monkeypatch, tmp_path: Path):
        """XDG_CONFIG_HOME env var is used when set."""
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "custom-config"))
        assert get_config_dir() == tmp_path / "custom-config" / "ewasd"


class TestToolConfig:
    """Verify config.toml parsing."""

    def test_missing_config_returns_empty(self, monkeypatch, tmp_path: Path):
        """No config.toml is fine - returns empty dict."""
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "nonexistent"))
        result = _read_tool_config()
        assert result == {}

    def test_config_toml_parsed(self, monkeypatch, tmp_path: Path):
        """Valid config.toml is parsed correctly."""
        config_dir = tmp_path / "config" / "ewasd"
        config_dir.mkdir(parents=True)
        (config_dir / "config.toml").write_text(
            'workspace = "/some/path"\nremote_keys = ["remote.origin.url", "remote.upstream.url"]\n'
        )
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
        result = _read_tool_config()
        assert result["workspace"] == "/some/path"
        assert result["remote_keys"] == ["remote.origin.url", "remote.upstream.url"]


class TestRemoteKeys:
    """Verify remote_keys is configurable instead of hardcoded."""

    def test_default_remote_keys(self, monkeypatch, tmp_path: Path):
        """Default is just remote.origin.url."""
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "nonexistent"))
        keys = get_remote_keys()
        assert keys == ["remote.origin.url"]

    def test_custom_remote_keys(self, monkeypatch, tmp_path: Path):
        """Config file can specify additional remote keys."""
        config_dir = tmp_path / "config" / "ewasd"
        config_dir.mkdir(parents=True)
        (config_dir / "config.toml").write_text(
            'remote_keys = ["remote.origin.url", "remote.emu.url"]\n'
        )
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
        keys = get_remote_keys()
        assert keys == ["remote.origin.url", "remote.emu.url"]


class TestConfigParserWorkspace:
    """Verify ConfigParser uses workspace_dir to resolve paths."""

    def test_workspace_dir_resolves_link_dir(self, tmp_path: Path):
        """link_dir in editors.toml is relative to workspace_dir, not package dir."""
        ws = tmp_path / "workspace"
        ws.mkdir()
        (ws / "repos" / "myproject").mkdir(parents=True)
        (ws / "editors.toml").write_text(
            "[repos]\n"
            "[repos.myproject]\n"
            'repo = "https://github.com/user/myproject.git"\n'
            'link_dir = "repos/myproject"\n'
        )

        cfg = ConfigParser(workspace_dir=ws)
        repo = cfg.get_repo("myproject")
        assert repo.link_dir == ws / "repos" / "myproject"

    def test_workspace_dir_not_package_dir(self, tmp_path: Path):
        """Workspace dir is independent of where ewasd package is installed."""
        # Workspace in one place
        ws = tmp_path / "my-data"
        ws.mkdir()
        (ws / "editors.toml").write_text(
            "[repos]\n"
            "[repos.test]\n"
            'repo = "https://example.com/test.git"\n'
            'link_dir = "repos/test"\n'
        )
        (ws / "repos" / "test").mkdir(parents=True)
        (ws / "repos" / "test" / ".clangd").write_text("---")

        cfg = ConfigParser(workspace_dir=ws)
        repo = cfg.get_repo("test")
        assert repo.link_dir == ws / "repos" / "test"
        assert ".clangd" in repo.get_configs()

    def test_create_repo_entry_writes_to_workspace(self, tmp_path: Path, monkeypatch):
        """New repo entries are written to the workspace editors.toml, not the package."""
        ws = tmp_path / "workspace"
        ws.mkdir()
        (ws / "editors.toml").write_text("[repos]\n")
        (ws / "repos").mkdir()

        monkeypatch.setattr(
            "ewasd.core.collect_remotes", lambda: ["https://github.com/user/newrepo.git"]
        )

        cfg = ConfigParser(workspace_dir=ws)
        repo = cfg.create_repo_entry("newrepo", tmp_path)
        assert repo is not None
        assert repo.link_dir.is_relative_to(ws)

        # Verify it was written to workspace toml
        text = (ws / "editors.toml").read_text()
        assert "newrepo" in text


class TestLegacyFallback:
    """Verify backwards compatibility during transition period."""

    def test_legacy_workspace_found(self, monkeypatch, tmp_path: Path):
        """If editors.toml exists next to package, use that as workspace (dev mode)."""
        # Simulate: no env, no config, no XDG dir exists, but legacy dir does
        monkeypatch.delenv("EWASD_WORKSPACE", raising=False)
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "no-config"))
        monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "no-data"))

        # Create legacy location
        legacy = tmp_path / "git" / "editor_workspaces"
        legacy.mkdir(parents=True)
        (legacy / "editors.toml").write_text("[repos]\n")

        monkeypatch.setenv("HOME", str(tmp_path))

        # Mock __file__ so the parent.parent fallback doesn't match
        monkeypatch.setattr(ewasd.core, "__file__", str(tmp_path / "fake" / "ewasd" / "core.py"))

        result = get_workspace_dir(cli_override=None)
        assert result == legacy
