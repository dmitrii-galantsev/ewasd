"""Tests for the structured API layer used by the MCP server."""

import json
import subprocess
from pathlib import Path

import pytest

from ewasd import api, core


def _git(path: Path, *args: str) -> None:
    subprocess.run(["git", "-C", str(path), *args], check=True, capture_output=True)


@pytest.fixture
def sandbox(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> tuple[Path, Path]:
    """A workspace with one repo, plus a matching git project. Returns (ws, project)."""
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "home" / ".config"))
    monkeypatch.delenv("EWASD_WORKSPACE", raising=False)

    ws = tmp_path / "ws"
    (ws / "repos" / "demo" / "nested").mkdir(parents=True)
    (ws / "editors.toml").write_text(
        '[repos]\n[repos.demo]\nrepo = "https://example.com/demo.git"\nlink_dir = "repos/demo"\n'
    )
    (ws / "repos" / "demo" / ".clangd").write_text("x\n")
    (ws / "repos" / "demo" / "AGENTS.md").write_text("y\n")
    (ws / "repos" / "demo" / "nested" / "note.md").write_text("z\n")

    proj = tmp_path / "projects" / "demo"
    proj.mkdir(parents=True)
    _git(proj, "init", "-q")
    _git(proj, "remote", "add", "origin", "https://example.com/demo.git")
    return ws, proj


class TestNoAmbientState:
    """The API must never depend on the process working directory."""

    def test_detect_uses_passed_cwd_not_process_cwd(self, sandbox, monkeypatch, tmp_path):
        ws, proj = sandbox
        # Run from a totally unrelated directory, as an MCP server would.
        monkeypatch.chdir(tmp_path)
        res = api.detect(cwd=proj, workspace=ws)
        assert res.ok
        assert res.repo_name == "demo"

    def test_collect_remotes_honours_cwd(self, sandbox):
        _ws, proj = sandbox
        assert core.collect_remotes(proj) == ["https://example.com/demo.git"]


class TestNoPrinting:
    """Core output must be captured, never written to stdout (it is the transport)."""

    def test_link_emits_nothing_to_stdout(self, sandbox, capsys):
        ws, proj = sandbox
        api.link(cwd=proj, workspace=ws, dry_run=False)
        captured = capsys.readouterr()
        assert captured.out == ""
        assert captured.err == ""

    def test_messages_are_returned_instead(self, sandbox):
        ws, proj = sandbox
        res = api.link(cwd=proj, workspace=ws, dry_run=False)
        assert res.messages
        assert {m["level"] for m in res.messages} <= {"info", "success", "warn"}

    def test_capture_messages_restores_previous_sink(self):
        with core.capture_messages() as outer:
            core.info("a")
            with core.capture_messages() as inner:
                core.warn("b")
            core.info("c")
        assert [m.text for m in inner] == ["b"]
        assert [m.text for m in outer] == ["a", "c"]


class TestErrorsAreValues:
    """Failures come back as data with recovery hints, not exceptions."""

    def test_missing_workspace(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        res = api.status(cwd=tmp_path, workspace=tmp_path / "nope")
        assert res.ok is False
        assert res.error == "no_workspace"
        assert res.hints

    def test_undetectable_repo(self, sandbox, tmp_path):
        ws, _proj = sandbox
        other = tmp_path / "projects" / "mystery"
        other.mkdir(parents=True)
        res = api.detect(cwd=other, workspace=ws)
        assert res.ok is False
        assert res.error == "repo_not_detected"
        assert res.known_repos == ["demo"]
        assert res.trace, "must explain which strategies were tried"

    def test_invalid_editors_toml(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        ws = tmp_path / "bad"
        ws.mkdir()
        (ws / "editors.toml").write_text('[repos]\n[repos.x]\nrepo = "u"\n')  # no link_dir
        res = api.list_configs(cwd=tmp_path, workspace=ws)
        assert res.ok is False
        assert res.error == "invalid_editors_toml"

    def test_add_files_rejects_empty_list(self, sandbox):
        ws, proj = sandbox
        res = api.add_files([], cwd=proj, workspace=ws)
        assert res.ok is False
        assert res.error == "no_files"


class TestDetectExplainability:
    def test_matched_by_remote(self, sandbox):
        ws, proj = sandbox
        res = api.detect(cwd=proj, workspace=ws)
        assert res.matched_by == "remote"
        assert res.trace, "success must be explained too"

    def test_matched_by_override(self, sandbox, tmp_path):
        ws, _proj = sandbox
        other = tmp_path / "elsewhere"
        other.mkdir()
        res = api.detect(cwd=other, workspace=ws, project="demo")
        assert res.repo_name == "demo"
        assert res.matched_by == "project_override"

    def test_matched_by_path_in_monorepo(self, sandbox, tmp_path):
        ws, _proj = sandbox
        mono = tmp_path / "mono"
        sub = mono / "components" / "demo"
        sub.mkdir(parents=True)
        _git(mono, "init", "-q")
        _git(mono, "remote", "add", "origin", "https://example.com/mono.git")
        res = api.detect(cwd=sub, workspace=ws)
        assert res.repo_name == "demo"
        assert res.matched_by == "path"


class TestLink:
    def test_dry_run_writes_nothing(self, sandbox):
        ws, proj = sandbox
        res = api.link(cwd=proj, workspace=ws, dry_run=True)
        assert res.ok
        assert sorted(res.linked) == [".clangd", "AGENTS.md", "nested"]
        assert not (proj / ".clangd").exists()
        assert res.gitignore is None

    def test_apply_creates_symlinks_and_gitignore(self, sandbox):
        ws, proj = sandbox
        res = api.link(cwd=proj, workspace=ws, dry_run=False)
        assert res.ok
        assert (proj / ".clangd").is_symlink()
        assert (proj / ".clangd").resolve() == (ws / "repos" / "demo" / ".clangd").resolve()
        assert res.gitignore is not None
        assert Path(res.gitignore).exists()

    def test_is_idempotent(self, sandbox):
        ws, proj = sandbox
        api.link(cwd=proj, workspace=ws, dry_run=False)
        second = api.link(cwd=proj, workspace=ws, dry_run=False)
        assert second.ok
        assert sorted(second.linked) == [".clangd", "AGENTS.md", "nested"]

    def test_existing_file_is_reported_not_clobbered(self, sandbox):
        ws, proj = sandbox
        (proj / "AGENTS.md").write_text("MINE\n")
        res = api.link(cwd=proj, workspace=ws, dry_run=False)
        assert (proj / "AGENTS.md").read_text() == "MINE\n"
        assert not (proj / "AGENTS.md").is_symlink()
        assert any("AGENTS.md" in s for s in res.skipped)


class TestStatusAndDoctor:
    def test_status_aggregates(self, sandbox):
        ws, proj = sandbox
        res = api.status(cwd=proj, workspace=ws)
        assert res.ok
        assert res.detection is not None and res.detection.repo_name == "demo"
        assert res.config is not None and res.config.known_repos == ["demo"]
        assert len(res.entries) == 3
        assert all(not e.linked for e in res.entries)
        assert "0/3" in res.summary

    def test_status_reflects_links(self, sandbox):
        ws, proj = sandbox
        api.link(cwd=proj, workspace=ws, dry_run=False)
        res = api.status(cwd=proj, workspace=ws)
        assert all(e.linked for e in res.entries)
        assert res.healthy is True

    def test_doctor_flags_orphaned_link(self, sandbox):
        """A link whose workspace source was deleted must still be reported.

        Regression: check_symlink_health only walks entries that still exist in
        the workspace, so deleting the source used to make the problem invisible.
        """
        ws, proj = sandbox
        api.link(cwd=proj, workspace=ws, dry_run=False)
        (ws / "repos" / "demo" / ".clangd").unlink()
        res = api.doctor(cwd=proj, workspace=ws)
        assert res.healthy is False
        orphan = [p for p in res.problems if p["path"].endswith(".clangd")]
        assert orphan and orphan[0]["reason"] == "orphaned"
        assert res.hints

    def test_doctor_flags_wrong_target(self, sandbox, tmp_path):
        ws, proj = sandbox
        api.link(cwd=proj, workspace=ws, dry_run=False)
        decoy = tmp_path / "decoy.md"
        decoy.write_text("nope\n")
        (proj / "AGENTS.md").unlink()
        (proj / "AGENTS.md").symlink_to(decoy)
        res = api.doctor(cwd=proj, workspace=ws)
        assert res.healthy is False
        assert any(p["reason"] == "wrong_target" for p in res.problems)


class TestAddFiles:
    def test_moves_and_symlinks_back(self, sandbox):
        ws, proj = sandbox
        (proj / ".editorconfig").write_text("root = true\n")
        res = api.add_files([".editorconfig"], cwd=proj, workspace=ws, project="demo")
        assert res.ok
        assert res.added == [".editorconfig"]
        assert (proj / ".editorconfig").is_symlink()
        assert (ws / "repos" / "demo" / ".editorconfig").read_text() == "root = true\n"

    def test_missing_file_reports_reason(self, sandbox):
        ws, proj = sandbox
        res = api.add_files(["ghost.txt"], cwd=proj, workspace=ws, project="demo")
        assert res.ok is False
        assert res.skipped[0]["reason"] == "missing_locally"

    def test_already_linked_is_distinguished_from_conflict(self, sandbox):
        ws, proj = sandbox
        (proj / "dup.txt").write_text("v1\n")
        api.add_files(["dup.txt"], cwd=proj, workspace=ws, project="demo")
        again = api.add_files(["dup.txt"], cwd=proj, workspace=ws, project="demo")
        assert again.skipped[0]["reason"] == "already_linked"


class TestSerialisation:
    def test_every_result_is_json_serialisable(self, sandbox):
        ws, proj = sandbox
        for result in (
            api.describe_config(ws),
            api.detect(proj, ws),
            api.list_configs(proj, ws),
            api.doctor(proj, ws),
            api.status(proj, ws),
            api.link(proj, ws, dry_run=True),
            api.add_files(["x"], proj, ws),
            api.git_clean_args(proj, ws),
        ):
            json.dumps(api.to_jsonable(result))

    def test_git_clean_args_is_advisory(self, sandbox):
        ws, proj = sandbox
        out = api.git_clean_args(proj, ws)
        assert out.ok is True
        assert out.command.startswith("git clean")
        assert core.GITIGNORE_FILENAME in out.tokens


class TestInit:
    def test_creates_workspace(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        ws = tmp_path / "fresh"
        res = api.init(workspace=ws)
        assert res.ok
        assert (ws / "editors.toml").exists()
        assert (ws / "repos").is_dir()

    def test_is_safe_to_repeat(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        ws = tmp_path / "fresh"
        api.init(workspace=ws)
        (ws / "editors.toml").write_text('[repos]\n[repos.keep]\nrepo="u"\nlink_dir="d"\n')
        res = api.init(workspace=ws)
        assert res.ok
        assert "keep" in (ws / "editors.toml").read_text()
