// Package config loads ewasd's optional user config file,
// $XDG_CONFIG_HOME/ewasd/config.toml (default ~/.config/ewasd/config.toml).
//
// This restores configuration surface the old Python ewasd had
// (get_config_dir / _read_tool_config / get_remote_keys in the original
// core.py) that the Go rewrite initially dropped. The supported keys keep
// their old names exactly:
//
//	workspace = "/path/to/workspace"
//	remote_keys = ["remote.origin.url", "remote.upstream.url"]
//
// A missing config file is not an error: Load returns a zero Config. A
// malformed one always is: this package never half-parses a file and
// silently proceeds with a partially-understood configuration. There is no
// TOML library available (go.mod is dependency-free by design), so
// toml.go hand-writes a small, strict subset of TOML sufficient for these
// two keys and rejects everything else with a clear, line-numbered error.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRemoteKeys is used when remote_keys is not set in config.toml. It
// matches the old Python get_remote_keys()'s default of a single key,
// origin's URL.
var DefaultRemoteKeys = []string{"remote.origin.url"}

// Config holds the parsed, but not yet defaulted, contents of config.toml.
// Callers decide how to fall back when a field is absent (Workspace == "",
// RemoteKeys == nil) because the right default depends on context (for
// example, the CLI wants to report *why* a value was chosen).
type Config struct {
	// Workspace is the "workspace" key, with a leading "~/" already
	// expanded, or "" if the key is absent.
	Workspace string
	// RemoteKeys is the "remote_keys" key, or nil if the key is absent.
	// Callers should fall back to DefaultRemoteKeys when nil.
	RemoteKeys []string
}

// Dir returns $XDG_CONFIG_HOME/ewasd (default ~/.config/ewasd). It creates
// nothing on disk; callers that need the directory to exist must create it
// themselves.
func Dir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ewasd")
}

// FilePath returns the path to ewasd's config.toml: Dir()/config.toml.
func FilePath() string {
	return filepath.Join(Dir(), "config.toml")
}

// Load reads and parses the config.toml at path.
//
// If the file does not exist, Load returns a zero Config, existed=false,
// and a nil error — a missing config file is a normal, supported state,
// not a failure.
//
// If the file exists but cannot be parsed, or uses a key with the wrong
// shape (e.g. workspace as an array, or remote_keys as a string), Load
// returns existed=true and a non-nil error naming path and the offending
// line. It never returns a partially-applied Config alongside an error.
func Load(path string) (cfg Config, existed bool, err error) {
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if readErr != nil {
		return Config{}, false, fmt.Errorf("read %s: %w", path, readErr)
	}
	values, parseErr := parseTOML(path, string(data))
	if parseErr != nil {
		return Config{}, true, parseErr
	}
	cfg, err = fromValues(path, values)
	if err != nil {
		return Config{}, true, err
	}
	return cfg, true, nil
}

// fromValues extracts the two supported keys from a parsed key/value map,
// enforcing that each has the shape ewasd expects.
func fromValues(path string, values map[string]tomlValue) (Config, error) {
	var cfg Config
	if v, ok := values["workspace"]; ok {
		if v.isArray {
			return Config{}, tomlErr(path, v.line, "workspace must be a quoted string, not an array")
		}
		cfg.Workspace = ExpandHome(v.str)
	}
	if v, ok := values["remote_keys"]; ok {
		if !v.isArray {
			return Config{}, tomlErr(path, v.line, "remote_keys must be an array of strings, not a string")
		}
		cfg.RemoteKeys = append([]string{}, v.array...)
	}
	return cfg, nil
}

// ExpandHome expands a leading "~" or "~/" in value to the current user's
// home directory. Values that don't start with "~" are returned unchanged.
func ExpandHome(value string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	return value
}
