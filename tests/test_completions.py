"""Tests for shell completion generators."""

from ewasd.completions import (
    COMPLETIONS,
    generate_bash_completion,
    generate_fish_completion,
    generate_zsh_completion,
)


class TestCompletionsData:
    """Verify the COMPLETIONS data structure is consistent."""

    def test_main_options_present(self):
        assert "--workspace" in COMPLETIONS["main_options"]
        assert "--project" in COMPLETIONS["main_options"]
        assert "--add-file" in COMPLETIONS["main_options"]

    def test_subcommands_present(self):
        expected = {"link", "list", "version", "git-clean-args", "clean", "init", "config", "migrate", "doctor", "completion"}
        assert set(COMPLETIONS["subcommands"].keys()) == expected

    def test_completion_shells(self):
        assert set(COMPLETIONS["completion_shells"]) == {"bash", "fish", "zsh"}


class TestBashCompletion:
    """Validate bash completion script."""

    def test_returns_string(self):
        result = generate_bash_completion()
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_function_definition(self):
        result = generate_bash_completion()
        assert "_ewasd_completion()" in result

    def test_contains_complete_command(self):
        result = generate_bash_completion()
        assert "complete -F _ewasd_completion ewasd" in result

    def test_contains_all_subcommands(self):
        result = generate_bash_completion()
        for cmd in COMPLETIONS["subcommands"]:
            assert cmd in result

    def test_contains_main_options(self):
        result = generate_bash_completion()
        for opt in COMPLETIONS["main_options"]:
            assert opt in result

    def test_contains_clean_options(self):
        result = generate_bash_completion()
        assert "--dry-run" in result
        assert "--force" in result
        assert "--directories" in result

    def test_contains_init_options(self):
        result = generate_bash_completion()
        assert "--from-git" in result

    def test_contains_migrate_options(self):
        result = generate_bash_completion()
        assert "--old-workspace" in result
        assert "--scan-dir" in result

    def test_completion_shell_choices(self):
        result = generate_bash_completion()
        for shell in COMPLETIONS["completion_shells"]:
            assert shell in result

    def test_directory_completion_for_workspace(self):
        result = generate_bash_completion()
        assert "--workspace" in result
        assert "compgen -d" in result

    def test_file_completion_for_add_file(self):
        result = generate_bash_completion()
        assert "--add-file" in result
        assert "compgen -f" in result


class TestFishCompletion:
    """Validate fish completion script."""

    def test_returns_string(self):
        result = generate_fish_completion()
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_complete_commands(self):
        result = generate_fish_completion()
        assert "complete -c ewasd" in result

    def test_contains_all_subcommands(self):
        result = generate_fish_completion()
        for cmd in COMPLETIONS["subcommands"]:
            assert cmd in result

    def test_subcommand_descriptions(self):
        result = generate_fish_completion()
        for desc in COMPLETIONS["subcommands"].values():
            assert desc in result

    def test_contains_main_options(self):
        result = generate_fish_completion()
        assert "--workspace" in result or "-l workspace" in result
        assert "--project" in result or "-l project" in result
        assert "--add-file" in result or "-l add-file" in result

    def test_contains_clean_options(self):
        result = generate_fish_completion()
        assert "dry-run" in result
        assert "force" in result
        assert "directories" in result

    def test_contains_init_options(self):
        result = generate_fish_completion()
        assert "from-git" in result

    def test_contains_migrate_options(self):
        result = generate_fish_completion()
        assert "old-workspace" in result
        assert "scan-dir" in result

    def test_uses_fish_subcommand_guard(self):
        result = generate_fish_completion()
        assert "__fish_use_subcommand" in result

    def test_uses_fish_seen_subcommand(self):
        result = generate_fish_completion()
        assert "__fish_seen_subcommand_from clean" in result
        assert "__fish_seen_subcommand_from init" in result
        assert "__fish_seen_subcommand_from migrate" in result

    def test_directory_completion_for_workspace(self):
        result = generate_fish_completion()
        assert "__fish_complete_directories" in result

    def test_install_option_for_completion(self):
        result = generate_fish_completion()
        assert "install" in result


class TestZshCompletion:
    """Validate zsh completion script."""

    def test_returns_string(self):
        result = generate_zsh_completion()
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_compdef(self):
        result = generate_zsh_completion()
        assert "#compdef ewasd" in result

    def test_contains_function_definition(self):
        result = generate_zsh_completion()
        assert "_ewasd()" in result

    def test_contains_all_subcommands(self):
        result = generate_zsh_completion()
        for cmd in COMPLETIONS["subcommands"]:
            assert cmd in result

    def test_subcommand_descriptions(self):
        result = generate_zsh_completion()
        for desc in COMPLETIONS["subcommands"].values():
            assert desc in result

    def test_contains_main_options(self):
        result = generate_zsh_completion()
        assert "--workspace" in result
        assert "--project" in result
        assert "--add-file" in result

    def test_contains_clean_options(self):
        result = generate_zsh_completion()
        assert "--dry-run" in result
        assert "--force" in result

    def test_contains_init_options(self):
        result = generate_zsh_completion()
        assert "--from-git" in result

    def test_contains_migrate_options(self):
        result = generate_zsh_completion()
        assert "--old-workspace" in result
        assert "--scan-dir" in result

    def test_uses_zsh_arguments(self):
        result = generate_zsh_completion()
        assert "_arguments" in result

    def test_uses_zsh_directories(self):
        result = generate_zsh_completion()
        assert "_directories" in result

    def test_uses_zsh_files(self):
        result = generate_zsh_completion()
        assert "_files" in result

    def test_commands_helper_function(self):
        result = generate_zsh_completion()
        assert "_ewasd_commands()" in result
        assert "_describe" in result
