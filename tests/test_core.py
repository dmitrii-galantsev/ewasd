from pathlib import Path

from ewasd.core import (
    Repo,
    build_git_clean_tokens,
    detect_repo_name,
    find_repo_name,
    find_repo_name_in_path,
)


def test_find_repo_name_variants():
    remotes = ["git@github.com:user/project.git", "https://example.com/another/project2"]
    assert find_repo_name(remotes) == "project"
    assert find_repo_name([remotes[1]]) == "project2"


def test_build_git_clean_tokens_order(tmp_path: Path):
    # Prepare fake repo directory contents
    repo_dir = tmp_path / "fake"
    repo_dir.mkdir()
    for name in [".clangd", ".gitignore", "README.md"]:
        (repo_dir / name).write_text("x")

    repo = Repo(name="fake", git_url="url", link_dir=repo_dir)
    tokens = build_git_clean_tokens(repo)
    # Expect 2 tokens per file plus 2 for .ewasd_gitignore
    assert len(tokens) == 8
    # Tokens should alternate -e <file>
    for i in range(0, len(tokens), 2):
        assert tokens[i] == "-e"


def test_find_repo_name_in_path_monorepo(tmp_path: Path):
    # Simulate monorepo root and nested project
    mono = tmp_path / "rocm-systems"
    proj = mono / "projects" / "rdc"
    proj.mkdir(parents=True)
    # Known repo names list includes rdc
    detected = find_repo_name_in_path(proj, ["rdc", "other"], git_root=mono)
    assert detected == "rdc"


def test_detect_repo_name_override(tmp_path: Path, monkeypatch):
    mono = tmp_path / "rocm-systems"
    proj = mono / "projects" / "rdc"
    proj.mkdir(parents=True)

    # Fake git root command
    def fake_check_output(cmd, cwd=None, text=None):
        if cmd[:2] == ["git", "rev-parse"]:
            return str(mono)
        raise RuntimeError("Unexpected command")

    monkeypatch.setattr("subprocess.check_output", fake_check_output)
    name = detect_repo_name(
        project_override=None,
        remotes=["https://example.com/rocm-systems.git"],
        cwd=proj,
        known_repo_names=["rdc", "foo"],
    )
    assert name == "rdc"
    # Override should win even if not in path
    name2 = detect_repo_name(
        project_override="foo",
        remotes=["https://example.com/rocm-systems.git"],
        cwd=proj,
        known_repo_names=["rdc", "foo"],
    )
    assert name2 == "foo"


def test_link_any_file(tmp_path: Path):
    """Test linking a single file."""
    # Setup source and target directories
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"
    src_dir.mkdir()
    dst_dir.mkdir()

    # Create a file in source
    (src_dir / "config.txt").write_text("content")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_any("config.txt", dst_dir)

    # Should return the filename
    assert result == ["config.txt"]
    # Should create symlink
    assert (dst_dir / "config.txt").is_symlink()
    assert (dst_dir / "config.txt").resolve() == (src_dir / "config.txt").resolve()


def test_link_any_existing_symlink(tmp_path: Path):
    """Test that existing symlinks are tracked but not recreated."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"
    src_dir.mkdir()
    dst_dir.mkdir()

    (src_dir / "config.txt").write_text("content")
    (dst_dir / "config.txt").symlink_to(src_dir / "config.txt")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_any("config.txt", dst_dir)

    # Should track existing symlink
    assert result == ["config.txt"]


def test_link_any_existing_file(tmp_path: Path):
    """Test that existing real files are skipped."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"
    src_dir.mkdir()
    dst_dir.mkdir()

    (src_dir / "config.txt").write_text("source content")
    (dst_dir / "config.txt").write_text("existing content")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_any("config.txt", dst_dir)

    # Should skip and return empty
    assert result == []
    # Original file should remain
    assert (dst_dir / "config.txt").read_text() == "existing content"


def test_link_directory_recursive(tmp_path: Path):
    """Test recursive directory linking."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"

    # Create nested structure in source
    (src_dir / "subdir").mkdir(parents=True)
    (src_dir / "subdir" / "file1.txt").write_text("content1")
    (src_dir / "subdir" / "file2.txt").write_text("content2")

    # Create matching directory structure in target (simulates real project dirs)
    (dst_dir / "subdir").mkdir(parents=True)
    (dst_dir / "subdir" / "existing.txt").write_text("existing")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_directory(src_dir / "subdir", dst_dir / "subdir", "subdir/")

    # Should return relative paths
    assert sorted(result) == ["subdir/file1.txt", "subdir/file2.txt"]

    # Symlinks should be created
    assert (dst_dir / "subdir" / "file1.txt").is_symlink()
    assert (dst_dir / "subdir" / "file2.txt").is_symlink()

    # Existing file should remain
    assert (dst_dir / "subdir" / "existing.txt").read_text() == "existing"
    assert not (dst_dir / "subdir" / "existing.txt").is_symlink()


def test_link_any_directory_recurse(tmp_path: Path):
    """Test link_any with directory that exists in both locations."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"

    # Create nested structure
    (src_dir / "server").mkdir(parents=True)
    (src_dir / "server" / "AGENT_CODE_INFO.md").write_text("docs")

    # Target has same directory with real files
    (dst_dir / "server").mkdir(parents=True)
    (dst_dir / "server" / "main.cpp").write_text("code")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_any("server", dst_dir)

    # Should return nested path
    assert result == ["server/AGENT_CODE_INFO.md"]

    # Should create symlink for doc file
    assert (dst_dir / "server" / "AGENT_CODE_INFO.md").is_symlink()

    # Should not touch existing code file
    assert not (dst_dir / "server" / "main.cpp").is_symlink()


def test_link_any_directory_whole(tmp_path: Path):
    """Test link_any with directory that doesn't exist in target."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"
    src_dir.mkdir()
    dst_dir.mkdir()

    # Create directory only in source
    (src_dir / "newdir").mkdir()
    (src_dir / "newdir" / "file.txt").write_text("content")

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    result = repo.link_any("newdir", dst_dir)

    # Should return directory name (not nested files)
    assert result == ["newdir"]

    # Should symlink whole directory
    assert (dst_dir / "newdir").is_symlink()


def test_link_all_tracks_all_symlinks(tmp_path: Path):
    """Test link_all collects all symlinked paths."""
    src_dir = tmp_path / "source"
    dst_dir = tmp_path / "target"
    src_dir.mkdir()
    dst_dir.mkdir()

    # Create mixed structure
    (src_dir / "file1.txt").write_text("a")
    (src_dir / "file2.txt").write_text("b")
    (src_dir / "subdir").mkdir()
    (src_dir / "subdir" / "nested.txt").write_text("c")

    # Target has matching subdir
    (dst_dir / "subdir").mkdir()

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    repo.link_all(dst_dir)

    # Check gitignore was created with all paths
    gitignore = (dst_dir / ".ewasd_gitignore").read_text()
    assert "file1.txt" in gitignore
    assert "file2.txt" in gitignore
    assert "subdir/nested.txt" in gitignore
    assert ".ewasd_gitignore" in gitignore


def test_update_gitignore_monorepo_paths(tmp_path: Path, monkeypatch):
    """Test gitignore uses relative paths for monorepo."""
    src_dir = tmp_path / "source"
    mono_root = tmp_path / "monorepo"
    project_dir = mono_root / "projects" / "myproject"

    src_dir.mkdir()
    project_dir.mkdir(parents=True)
    (src_dir / "config.txt").write_text("content")

    # Mock git root detection
    def fake_check_output(cmd, cwd=None, text=None):
        if cmd[:2] == ["git", "rev-parse"]:
            return str(mono_root)
        if cmd[:3] == ["git", "config", "core.excludesFile"]:
            return ""
        raise RuntimeError(f"Unexpected command: {cmd}")

    monkeypatch.setattr("subprocess.check_output", fake_check_output)
    monkeypatch.setattr("subprocess.run", lambda *args, **kwargs: None)

    repo = Repo(name="test", git_url="url", link_dir=src_dir)
    repo.update_gitignore(project_dir, ["config.txt", "subdir/file.md"])

    # Check gitignore has prefixed paths
    gitignore = (project_dir / ".ewasd_gitignore").read_text()
    assert "projects/myproject/.ewasd_gitignore" in gitignore
    assert "projects/myproject/config.txt" in gitignore
    assert "projects/myproject/subdir/file.md" in gitignore
