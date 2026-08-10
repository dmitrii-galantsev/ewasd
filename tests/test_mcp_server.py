"""Contract tests for the MCP server surface.

These assert the things an MCP client relies on: tool names, annotations,
schemas, and that every tool returns parseable JSON. They do not need a
running transport.
"""

import importlib
import json
from pathlib import Path

import pytest

from ewasd import api

# Guarded: the Nix build and a plain `pip install ewasd` have no MCP SDK, and
# anyio arrives with it. Both must be resolved before any use at module scope.
mcp_server = pytest.importorskip("ewasd.mcp_server", reason="requires the 'mcp' extra")
anyio = pytest.importorskip("anyio", reason="requires the 'mcp' extra")

READ_ONLY_TOOLS = {
    "status",
    "describe_config",
    "detect",
    "list_configs",
    "doctor",
    "git_clean_args",
}
WRITE_TOOLS = {"link", "add_files", "init"}


@pytest.fixture
def tools() -> dict:
    listed = anyio.run(mcp_server.mcp.list_tools)
    return {t.name: t for t in listed}


class TestToolSurface:
    def test_expected_tools_are_registered(self, tools):
        assert set(tools) == READ_ONLY_TOOLS | WRITE_TOOLS

    def test_every_tool_has_a_description(self, tools):
        for name, tool in tools.items():
            assert tool.description, f"{name} has no description"
            assert len(tool.description) > 40, f"{name} description is too thin to be useful"

    def test_read_tools_are_annotated_read_only(self, tools):
        for name in READ_ONLY_TOOLS:
            ann = tools[name].annotations
            assert ann is not None, f"{name} is missing annotations"
            assert _hint(ann, "read_only") is True, f"{name} must be marked read-only"

    def test_write_tools_are_not_marked_read_only(self, tools):
        for name in WRITE_TOOLS:
            ann = tools[name].annotations
            assert ann is not None
            assert _hint(ann, "read_only") is False

    def test_add_files_is_not_idempotent(self, tools):
        """add_files MOVES the user's files, so re-running is not a no-op."""
        assert _hint(tools["add_files"].annotations, "idempotent") is False

    def test_add_files_warns_about_moving_in_its_description(self, tools):
        assert "MOVE" in tools["add_files"].description.upper()

    def test_strict_mode_marks_add_files_destructive(self, monkeypatch):
        """EWASD_MCP_STRICT=1 lets a deployment force human approval for moves.

        Verified against Codex: with destructiveHint=true and
        approval_policy="never" the client auto-denies the call.
        """
        monkeypatch.setenv("EWASD_MCP_STRICT", "1")
        reloaded = importlib.reload(mcp_server)
        try:
            assert _hint(reloaded.MOVE, "destructive") is True
        finally:
            monkeypatch.delenv("EWASD_MCP_STRICT")
            importlib.reload(mcp_server)

    def test_link_defaults_to_dry_run(self, tools):
        schema = _schema(tools["link"])
        assert schema["properties"]["dry_run"]["default"] is True

    def test_cwd_is_accepted_everywhere_it_matters(self, tools):
        for name in READ_ONLY_TOOLS | {"link", "add_files"}:
            if name == "describe_config":
                continue
            assert "cwd" in _schema(tools[name])["properties"], f"{name} cannot target a dir"

    def test_add_files_requires_files(self, tools):
        assert _schema(tools["add_files"]).get("required") == ["files"]

    def test_server_has_instructions(self):
        assert mcp_server.mcp.instructions
        assert "dry_run" in mcp_server.mcp.instructions


def _schema(tool: object) -> dict:
    """Read the input schema across SDK 1.x (inputSchema) and 2.x (input_schema)."""
    return getattr(tool, "input_schema", None) or tool.inputSchema  # type: ignore[attr-defined]


def _hint(annotations: object, kind: str) -> object:
    """Read a hint across SDK 1.x (camelCase) and 2.x (snake_case) field names."""
    for attr in (f"{kind}_hint", f"{kind}Hint".replace("read_only", "readOnly")):
        if hasattr(annotations, attr):
            return getattr(annotations, attr)
    camel = {"read_only": "readOnlyHint", "destructive": "destructiveHint"}
    camel["idempotent"] = "idempotentHint"
    return getattr(annotations, camel[kind])


class TestStructuredOutput:
    """MCP structured output (spec 2025-06-18+): tools must publish an outputSchema."""

    def test_every_tool_publishes_an_output_schema(self, tools):
        for name, tool in tools.items():
            schema = getattr(tool, "output_schema", None) or getattr(tool, "outputSchema", None)
            assert schema, f"{name} has no outputSchema"
            props = schema.get("properties", {})
            assert props != {"result": {"title": "Result", "type": "string"}}, (
                f"{name} returns an opaque string, not structured data"
            )
            assert "ok" in props, f"{name} outputSchema is missing the 'ok' envelope field"

    def test_error_envelope_is_in_the_schema(self, tools):
        """Clients can see the failure shape without having to call the tool."""
        props = _out_schema(tools["status"])["properties"]
        for key in ("ok", "error", "detail", "hints", "messages"):
            assert key in props

    def test_tools_have_titles(self, tools):
        for name, tool in tools.items():
            title = getattr(tool, "title", None) or (
                tool.annotations.title if tool.annotations else None
            )
            assert title, f"{name} has no human-readable title"


def _out_schema(tool: object) -> dict:
    return getattr(tool, "output_schema", None) or tool.outputSchema  # type: ignore[attr-defined]


class TestToolOutput:
    def test_tools_return_json(self, tmp_path: Path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        ws = tmp_path / "ws"
        (ws / "repos" / "demo").mkdir(parents=True)
        (ws / "editors.toml").write_text(
            '[repos]\n[repos.demo]\nrepo = "u"\nlink_dir = "repos/demo"\n'
        )
        (ws / "repos" / "demo" / ".clangd").write_text("x\n")
        proj = tmp_path / "demo"
        proj.mkdir()

        for result in (
            mcp_server.describe_config(workspace=str(ws)),
            mcp_server.status(cwd=str(proj), workspace=str(ws), project="demo"),
            mcp_server.detect(cwd=str(proj), workspace=str(ws), project="demo"),
            mcp_server.list_configs(cwd=str(proj), workspace=str(ws), project="demo"),
            mcp_server.doctor(cwd=str(proj), workspace=str(ws), project="demo"),
            mcp_server.git_clean_args(cwd=str(proj), workspace=str(ws), project="demo"),
            mcp_server.link(cwd=str(proj), workspace=str(ws), project="demo", dry_run=True),
        ):
            parsed = json.loads(json.dumps(api.to_jsonable(result)))
            assert "ok" in parsed

    def test_errors_are_returned_not_raised(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        res = mcp_server.status(cwd=str(tmp_path), workspace=str(tmp_path / "none"))
        assert res.ok is False
        assert res.error
        assert res.hints

    def test_no_stdout_pollution(self, tmp_path, monkeypatch, capsys):
        """stdout is the JSON-RPC transport; a stray print corrupts the session."""
        monkeypatch.setenv("HOME", str(tmp_path / "home"))
        ws = tmp_path / "ws"
        (ws / "repos" / "demo").mkdir(parents=True)
        (ws / "editors.toml").write_text(
            '[repos]\n[repos.demo]\nrepo = "u"\nlink_dir = "repos/demo"\n'
        )
        (ws / "repos" / "demo" / ".clangd").write_text("x\n")
        proj = tmp_path / "demo"
        proj.mkdir()
        mcp_server.link(cwd=str(proj), workspace=str(ws), project="demo", dry_run=False)
        captured = capsys.readouterr()
        assert captured.out == ""
        assert captured.err == ""
