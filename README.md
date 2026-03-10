# ewasd

EWasd - **E**ditor **W**orkspaces

(ewasd is simple to type)

Symlink configuration files (dotfiles, build scripts, etc.) into your project directories based on git repository detection. Keep your configs centralized while avoiding repository pollution.

## Quick Start

```bash
# Install
pip install ewasd    # or: pip install -e .

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
