"""Shell completion templates for ewasd."""

# Completion data - used to generate shell-specific completions
COMPLETIONS = {
    "main_options": ["--workspace", "--project", "--add-file"],
    "subcommands": {
        "link": "Link all discovered config entries (default action)",
        "list": "List config entries for the detected repo",
        "version": "Show version information",
        "git-clean-args": "Emit -e tokens for git clean",
        "clean": "Run git clean while preserving linked configs",
        "init": "Initialize a new ewasd workspace",
        "config": "Show resolved configuration paths and settings",
        "migrate": "Fix broken symlinks after workspace relocation",
        "completion": "Generate shell completion scripts",
    },
    "link_options": ["--dry-run", "-n"],
    "clean_options": ["--dry-run", "--directories", "--force", "--extra", "-n", "-d", "-f"],
    "init_options": ["--from-git"],
    "migrate_options": ["--old-workspace", "--scan-dir", "--dry-run", "-n"],
    "completion_shells": ["bash", "fish", "zsh"],
}


def generate_bash_completion() -> str:
    """Generate bash completion script."""
    opts = " ".join(COMPLETIONS["main_options"])
    subs = " ".join(COMPLETIONS["subcommands"].keys())
    link_opts = " ".join(COMPLETIONS["link_options"])
    clean_opts = " ".join(COMPLETIONS["clean_options"])
    init_opts = " ".join(COMPLETIONS["init_options"])
    migrate_opts = " ".join(COMPLETIONS["migrate_options"])
    shells = " ".join(COMPLETIONS["completion_shells"])

    return f'''
_ewasd_completion() {{
    local cur prev opts subcommands
    COMPREPLY=()
    cur="${{COMP_WORDS[COMP_CWORD]}}"
    prev="${{COMP_WORDS[COMP_CWORD-1]}}"

    opts="{opts}"
    subcommands="{subs}"

    case "${{prev}}" in
        completion)
            COMPREPLY=( $(compgen -W "{shells}" -- "${{cur}}") )
            return 0
            ;;
        --add-file)
            COMPREPLY=( $(compgen -f -- "${{cur}}") )
            return 0
            ;;
        --workspace|--old-workspace|--scan-dir)
            COMPREPLY=( $(compgen -d -- "${{cur}}") )
            return 0
            ;;
        --from-git)
            return 0
            ;;
        link)
            COMPREPLY=( $(compgen -W "{link_opts}" -- "${{cur}}") )
            return 0
            ;;
        clean)
            COMPREPLY=( $(compgen -W "{clean_opts}" -- "${{cur}}") )
            return 0
            ;;
        init)
            COMPREPLY=( $(compgen -W "{init_opts}" -- "${{cur}}") )
            return 0
            ;;
        migrate)
            COMPREPLY=( $(compgen -W "{migrate_opts}" -- "${{cur}}") )
            return 0
            ;;
    esac

    if [[ ${{COMP_CWORD}} -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "${{opts}} ${{subcommands}}" -- "${{cur}}") )
        return 0
    fi

    local subcommand=""
    for i in $(seq 1 $((COMP_CWORD-1))); do
        case "${{COMP_WORDS[i]}}" in
            {"|".join(COMPLETIONS["subcommands"].keys())})
                subcommand="${{COMP_WORDS[i]}}"
                break
                ;;
        esac
    done

    if [[ -n "$subcommand" ]]; then
        case "$subcommand" in
            completion)
                if [[ ${{cur}} != -* ]]; then
                    COMPREPLY=( $(compgen -W "{shells} --install" -- "${{cur}}") )
                fi
                ;;
            link)
                COMPREPLY=( $(compgen -W "{link_opts}" -- "${{cur}}") )
                ;;
            clean)
                COMPREPLY=( $(compgen -W "{clean_opts}" -- "${{cur}}") )
                ;;
            init)
                COMPREPLY=( $(compgen -W "{init_opts}" -- "${{cur}}") )
                ;;
            migrate)
                COMPREPLY=( $(compgen -W "{migrate_opts}" -- "${{cur}}") )
                ;;
        esac
    else
        COMPREPLY=( $(compgen -W "${{opts}} ${{subcommands}}" -- "${{cur}}") )
    fi
}}

complete -F _ewasd_completion ewasd
'''


def generate_fish_completion() -> str:
    """Generate fish completion script."""
    shells = " ".join(COMPLETIONS["completion_shells"])

    lines = ["# Fish completion for ewasd", ""]

    # Main options
    lines.extend(
        [
            "complete -c ewasd -l workspace -d 'Path to ewasd workspace directory' -r -a '(__fish_complete_directories)'",
            "complete -c ewasd -l project -d 'Explicitly specify the project name'",
            "complete -c ewasd -l add-file -d 'Move file(s) to central repo and create symlink' -F",
            "",
        ]
    )

    # Subcommands
    for cmd, desc in COMPLETIONS["subcommands"].items():
        lines.append(f"complete -c ewasd -f -n '__fish_use_subcommand' -a '{cmd}' -d '{desc}'")
    lines.append("")

    # Link options
    lines.extend(
        [
            "complete -c ewasd -f -n '__fish_seen_subcommand_from link' -s n -l dry-run -d 'Show what would be created without making changes'",
            "",
        ]
    )

    # Clean options
    lines.extend(
        [
            "complete -c ewasd -f -n '__fish_seen_subcommand_from clean' -s n -l dry-run -d 'Show what would be removed'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from clean' -s d -l directories -d 'Include directories'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from clean' -s f -l force -d 'Force operation'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from clean' -l extra -d 'Extra arguments for git clean'",
            "",
        ]
    )

    # Init options
    lines.extend(
        [
            "complete -c ewasd -f -n '__fish_seen_subcommand_from init' -l from-git -d 'Clone workspace from a git repository URL'",
            "",
        ]
    )

    # Migrate options
    lines.extend(
        [
            "complete -c ewasd -f -n '__fish_seen_subcommand_from migrate' -l old-workspace -d 'Path to old workspace directory' -r -a '(__fish_complete_directories)'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from migrate' -l scan-dir -d 'Directory to scan for broken symlinks' -r -a '(__fish_complete_directories)'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from migrate' -s n -l dry-run -d 'Show what would be fixed without making changes'",
            "",
        ]
    )

    # Completion options
    lines.extend(
        [
            f"complete -c ewasd -f -n '__fish_seen_subcommand_from completion' -a '{shells}' -d 'Shell type'",
            "complete -c ewasd -f -n '__fish_seen_subcommand_from completion' -l install -d 'Install completion to standard location'",
        ]
    )

    return "\n".join(lines)


def generate_zsh_completion() -> str:
    """Generate zsh completion script."""
    subcommands = []
    for cmd, desc in COMPLETIONS["subcommands"].items():
        subcommands.append(f"'{cmd}:{desc}'")

    shells = " ".join(COMPLETIONS["completion_shells"])

    return f"""#compdef ewasd

_ewasd() {{
    local context curcontext="$curcontext" state line
    typeset -A opt_args

    _arguments -C \\
        '--workspace[Path to ewasd workspace directory]:workspace dir:_directories' \\
        '--project[Explicitly specify the project name]:project name:' \\
        '*--add-file[Move file(s) to central repo and create symlink]:file:_files' \\
        '1: :_ewasd_commands' \\
        '*::arg:->args' && return 0

    case $state in
        args)
            case $line[1] in
                completion)
                    _arguments \\
                        '--install[Install completion to standard location]' \\
                        '1:shell:({shells})'
                    ;;
                link)
                    _arguments \\
                        '(-n --dry-run){{-n,--dry-run}}'[Show what would be created without making changes]'
                    ;;
                clean)
                    _arguments \\
                        '(-n --dry-run){{-n,--dry-run}}'[Show what would be removed]' \\
                        '(-d --directories){{-d,--directories}}'[Include directories]' \\
                        '(-f --force){{-f,--force}}'[Force operation]' \\
                        '--extra[Extra arguments for git clean]:extra args:'
                    ;;
                init)
                    _arguments \\
                        '--from-git[Clone workspace from a git repository URL]:git url:'
                    ;;
                migrate)
                    _arguments \\
                        '--old-workspace[Path to old workspace directory]:old workspace dir:_directories' \\
                        '--scan-dir[Directory to scan for broken symlinks]:scan dir:_directories' \\
                        '(-n --dry-run){{-n,--dry-run}}'[Show what would be fixed without making changes]'
                    ;;
            esac
            ;;
    esac
}}

_ewasd_commands() {{
    local commands
    commands=({" ".join(subcommands)})
    _describe 'commands' commands
}}

_ewasd "$@"
"""
