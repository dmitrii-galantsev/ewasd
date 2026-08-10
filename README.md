# ewasd

EWasd - **E**ditor **W**orkspaces

(ewasd is simple to type)

Symlink configuration files (dotfiles, build scripts, etc.) into your project directories based on git repository detection. Keep your configs centralized while avoiding repository pollution.

## Install

```bash
# Nix (recommended)
nix profile add github:dmitrii-galantsev/ewasd

# pip
pip install git+https://github.com/dmitrii-galantsev/ewasd.git
```

## Quick Start

```bash

# Initialize a workspace
ewasd init                          # creates ~/.local/share/ewasd/
ewasd init --from-git <url>         # clone existing workspace

# Link configs for current repo
cd ~/my_project && ewasd link

# Clean while preserving symlinks
ewasd clean --dry-run
ewasd clean
```

## Workspace Layout

ewasd separates the **tool** (this package) from your **workspace data** (config files). The workspace lives at `$XDG_DATA_HOME/ewasd/` by default (`~/.local/share/ewasd/`):

```
~/.local/share/ewasd/
  editors.toml         # repo definitions
  repos/
    vendor/project/    # config files to symlink
```

### Workspace Resolution (priority order)

1. `--workspace PATH` CLI flag
2. `EWASD_WORKSPACE` environment variable
3. `workspace` key in `~/.config/ewasd/config.toml`
4. `$XDG_DATA_HOME/ewasd/` (default)

## Commands

* `init` - Initialize a new workspace
* `link` - Create symlinks from central configs to current directory (default)
* `list` - Show available configs for detected repository
* `config` - Show resolved configuration paths
* `migrate` - Fix broken symlinks after workspace relocation
* `clean` - Run `git clean` while preserving symlinked configs
* `git-clean-args` - Output exclusion args for manual `git clean`
* `completion` - Generate shell completions

## Configuration

### Workspace (`editors.toml`)

Define repositories and their config directories:

```toml
[repos.my_project]
repo = "https://github.com/user/my_project.git"
link_dir = "repos/my_project"
```

Place config files in the `link_dir` -they'll be symlinked when working in that repository.

### Tool Config (`~/.config/ewasd/config.toml`, optional)

```toml
# Override default workspace location
# workspace = "/home/user/my-dotfiles"

# Git remote keys to check for repo detection (default: ["remote.origin.url"])
# remote_keys = ["remote.origin.url", "remote.upstream.url"]
```

## MCP Server (AI agents)

ewasd ships an [MCP](https://modelcontextprotocol.io) server so agents can inspect and
manage links without shelling out and parsing text.

```bash
pip install 'ewasd[mcp]'   # adds the `ewasd-mcp` stdio server
```

Register it with your client.

**Codex** (`~/.codex/config.toml`) / **Claude Desktop**:

```toml
[mcp_servers.ewasd]
command = "ewasd-mcp"
# Optional: pin the workspace instead of relying on XDG resolution
[mcp_servers.ewasd.env]
EWASD_WORKSPACE = "/home/you/.local/share/ewasd"
```

**opencode** (`opencode.json`):

```json
{
  "mcp": {
    "ewasd": {
      "type": "local",
      "command": ["ewasd-mcp"],
      "enabled": true,
      "environment": { "EWASD_WORKSPACE": "/home/you/.local/share/ewasd" }
    }
  }
}
```

Verify with `opencode mcp list` or `codex mcp list`.

### Tools

| Tool | Kind | Purpose |
|------|------|---------|
| `status` | read | One-call orientation: config, detected repo, entries, link health |
| `describe_config` | read | Resolved workspace / editors.toml / known repo names |
| `detect` | read | Which repo matches a directory, and **why** (`matched_by` + `trace`) |
| `list_configs` | read | Managed entries with their current linked state |
| `doctor` | read | Broken, mis-pointed and orphaned symlinks |
| `git_clean_args` | read | Builds a safe `git clean` command — never runs it |
| `link` | write | Create symlinks; **defaults to `dry_run=true`** |
| `add_files` | write | Move file(s) into the workspace and symlink back |
| `init` | write | Create or clone a workspace |

Every tool takes an explicit `cwd` and reports failures as data (`ok: false` plus an
`error` code and actionable `hints`) rather than raising.

### Protocol

Built on the official Python SDK and compatible with both the 1.x (`FastMCP`) and 2.x
(`MCPServer`) releases; the protocol revision is negotiated by the SDK.

Tools use **structured output** — each publishes an `outputSchema` and returns
`structuredContent`, with a JSON text block alongside it for clients that predate the
feature. So a client knows the shape of a reply, including the error envelope, before
it ever makes a call.

`clean` is intentionally not exposed: `git clean` deletes untracked files and is not
something an agent should be able to trigger. Use `git_clean_args` and run it yourself.

`add_files` *moves* files. It is advertised as non-destructive by default so that
clients configured to auto-deny destructive tools can still use it. Set
`EWASD_MCP_STRICT=1` to mark it destructive and force those clients to ask a human.

### Library use

The same operations are available as a plain Python API that never prints:

```python
from ewasd import api

res = api.status(cwd="/path/to/repo")
print(res.summary, res.detection.matched_by)
```

## Shell Completions

```bash
# Install tab completion
ewasd completion bash --install   # or fish, zsh
```

## Options

* `--workspace PATH` - Override workspace location
* `--project NAME` - Override repository detection
* `--add-file FILE` - Move file to central location and create symlink back

## Examples

```bash
# Link configs automatically
cd ~/my_project && ewasd link

# Override detection for monorepos
ewasd --project my_component link

# Add new config file to central management
ewasd --add-file .clangd

# Use a custom workspace
ewasd --workspace ~/my-dotfiles link

# Show resolved paths
ewasd config

# Clean build artifacts safely
ewasd clean --dry-run --force
```
