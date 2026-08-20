// Package completion implements `ewasd completion` and the hidden
// `ewasd __complete` helper that back bash, zsh, and fish tab completion.
//
// `ewasd completion [bash|fish|zsh] [--install]` prints (or installs) a
// shell script for the requested shell, defaulting to whatever $SHELL says
// when no shell is named. Every generated script is intentionally thin: it
// handles only shell-specific plumbing — turning the words already on the
// command line into an `ewasd __complete ...` call, recognizing a special
// "please do directory completion here" marker, and feeding the resulting
// candidates back into that shell's completion machinery — and delegates
// every actual completion *decision* (which verb, which flag, which
// dynamic value) to the hidden `ewasd __complete` subcommand. That keeps
// three shell templates in sync with the CLI automatically instead of
// hand-maintaining three copies of the same logic.
//
// `__complete` is deliberately omitted from `ewasd help`/usage() output
// because it is a machine-facing contract between ewasd and the generated
// scripts, not a command a human is expected to type directly. Its
// contract:
//
//	ewasd __complete <word0> <word1> ... <wordN>
//
// <word0..N-1> are the command-line words already typed after "ewasd";
// <wordN> is the word currently being completed (frequently the empty
// string, e.g. immediately after a space). __complete prints zero or more
// newline-separated completion candidates to stdout and always exits 0.
//
// A literal candidate line of "__ewasd_dirs__" is a marker telling the
// calling script to also run its own native directory completion (used for
// --root, where ewasd wants to suggest both registered project roots and
// ordinary filesystem browsing, without ewasd itself having to reimplement
// a directory walker three different ways).
//
// __complete never prints to stderr, never prints a Go error string, and
// never blocks indefinitely: any failure to read state, resolve a project,
// or walk a directory tree simply yields no candidates for that class of
// value, and the whole call is bounded by an internal timeout. This is a
// hard requirement, not a nicety — this binary is invoked directly from a
// live shell's TAB key, and a completion helper that hangs, errors, or
// spews text onto the command line makes the shell itself feel broken.
package completion

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Shells lists the shells ewasd can generate completion scripts for, in the
// order they are offered as completion candidates.
var Shells = []string{"bash", "fish", "zsh"}

// Run implements the user-facing `ewasd completion [bash|fish|zsh]
// [--install]` command. Unlike __complete, Run is allowed to fail loudly:
// it is invoked directly by a human, not wired into a shell's TAB key.
//
// --install and the shell name are accepted in either order (e.g. both
// "ewasd completion bash --install" and "ewasd completion --install bash"
// work), which is why this parses args by hand instead of using
// flag.FlagSet: the standard flag package stops recognizing flags as soon
// as it sees the first positional argument.
func Run(args []string, getenv func(string) string, stdout io.Writer) error {
	install := false
	var positional []string
	for _, arg := range args {
		switch {
		case arg == "--install":
			install = true
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("completion: unknown flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) > 1 {
		return fmt.Errorf("completion accepts at most one shell argument (%s)", joinShells())
	}
	shell := ""
	if len(positional) == 1 {
		shell = positional[0]
	}
	if shell == "" {
		detected, err := DetectShell(getenv)
		if err != nil {
			return err
		}
		shell = detected
	}
	script, err := Script(shell)
	if err != nil {
		return err
	}
	if !install {
		_, err := io.WriteString(stdout, script)
		return err
	}
	path, hint, err := Install(shell, getenv, script)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s completion to %s\n", shell, path)
	if hint != "" {
		fmt.Fprintln(stdout, hint)
	}
	return nil
}

// DetectShell guesses the caller's shell from $SHELL (via getenv, so tests
// can inject a fake environment). It returns an error listing the valid
// choices when $SHELL is unset or names an unsupported shell.
func DetectShell(getenv func(string) string) (string, error) {
	shellPath := getenv("SHELL")
	name := filepath.Base(shellPath)
	for _, candidate := range Shells {
		if name == candidate {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot detect a supported shell from $SHELL=%q; pass one explicitly: %s", shellPath, joinShells())
}

// Script returns the generated completion script for shell, or an error
// listing the valid choices.
func Script(shell string) (string, error) {
	switch shell {
	case "bash":
		return GenerateBash(), nil
	case "fish":
		return GenerateFish(), nil
	case "zsh":
		return GenerateZsh(), nil
	default:
		return "", fmt.Errorf("unknown shell %q: expected one of %s", shell, joinShells())
	}
}

// Install writes script to the conventional completion path for shell,
// creating parent directories as needed, and returns the path written plus
// any activation step the user still needs to take (empty if none). Paths
// honour $HOME, $XDG_DATA_HOME, and $XDG_CONFIG_HOME via getenv so this is
// testable under a temporary HOME.
func Install(shell string, getenv func(string) string, script string) (path string, activationHint string, err error) {
	home := getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	dataHome := getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	configHome := getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}

	switch shell {
	case "bash":
		path = filepath.Join(dataHome, "bash-completion", "completions", "ewasd")
	case "zsh":
		dir := filepath.Join(dataHome, "zsh", "site-functions")
		path = filepath.Join(dir, "_ewasd")
		activationHint = fmt.Sprintf("ensure %s is on your zsh $fpath before compinit runs (e.g. `fpath+=(%s)` in ~/.zshrc), then start a new shell", dir, dir)
	case "fish":
		path = filepath.Join(configHome, "fish", "completions", "ewasd.fish")
	default:
		return "", "", fmt.Errorf("unknown shell %q: expected one of %s", shell, joinShells())
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", fmt.Errorf("create completion directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return "", "", fmt.Errorf("write completion script: %w", err)
	}
	return path, activationHint, nil
}

func joinShells() string {
	out := ""
	for i, shell := range Shells {
		if i > 0 {
			out += ", "
		}
		out += shell
	}
	return out
}
