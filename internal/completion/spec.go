package completion

// Verbs lists ewasd's top-level subcommands, in the order they are offered
// as completion candidates for the first command-line word. It deliberately
// excludes "__complete": that helper is a machine-facing implementation
// detail of completion itself, never something a human should tab-complete
// their way into typing.
//
// This list, and verbSpecs below, must stay in sync with the switch in
// cmd/ewasd/main.go's run(). There is no reflection-based way to derive
// them from the flag.NewFlagSet blocks there, so keep the two in lockstep
// by hand when the CLI surface changes.
var Verbs = []string{
	"config", "init", "register", "detect", "link", "clean", "unregister",
	"status", "adopt", "detach", "reconcile", "migrate", "recover", "mcp", "completion",
	"help", "version",
}

// dirsMarker is emitted as its own candidate line to tell a generated shell
// script "also offer this shell's native directory completion here", in
// addition to (not instead of) any literal candidates on the other lines.
const dirsMarker = "__ewasd_dirs__"

// positionalKind describes what, if anything, a verb's positional argument
// (i.e. an argument that isn't a flag or a flag's value) completes to.
type positionalKind int

const (
	posNone positionalKind = iota
	posShell
	posDetachPath
	posAdoptPath
)

// flagDef is one flag a verb accepts, for the purposes of flag-name
// completion (offering "--foo" once the user has typed "-"). Whether the
// flag also has a dynamically resolved value is decided separately, by
// flag *name* in resolveDynamicFlag — the same flag name always means the
// same kind of value everywhere it appears, so no per-verb bookkeeping is
// needed there.
type flagDef struct {
	name       string
	takesValue bool
}

type verbSpec struct {
	flags         []flagDef
	positional    positionalKind
	maxPositional int
}

var jsonFlag = flagDef{"--json", false}

// workspaceFlag is the global "--workspace PATH" flag (see cmd/ewasd's
// extractWorkspaceFlag). It is accepted by every subcommand that builds a
// store — every verb below except "completion", "help", and "version" —
// both before and after the verb name, so it is listed in each of their
// flag sets here for "--" flag-name completion. Its dynamically resolved
// value (directory completion, via dirsMarker) is wired in complete.go's
// resolveDynamicFlag, the same way --root's is.
var workspaceFlag = flagDef{"--workspace", true}

var verbSpecs = map[string]verbSpec{
	"config": {flags: []flagDef{
		workspaceFlag, jsonFlag,
	}},
	"init": {flags: []flagDef{
		workspaceFlag, {"--from-git", true},
	}},
	"register": {flags: []flagDef{
		workspaceFlag, {"--root", true}, {"--name", true}, jsonFlag,
	}},
	"detect": {flags: []flagDef{
		workspaceFlag, {"--root", true}, {"--project", true}, jsonFlag,
	}},
	"link": {flags: []flagDef{
		workspaceFlag, {"--root", true}, {"--project", true}, {"--dry-run", false}, {"-n", false}, jsonFlag,
	}},
	"clean": {flags: []flagDef{
		workspaceFlag, {"--root", true}, {"--project", true}, {"--dry-run", false}, {"-n", false},
		{"--apply", false}, {"--revision", true}, {"--fingerprint", true},
		{"--directories", false}, {"-d", false}, {"--mode", true}, jsonFlag,
	}},
	"unregister": {flags: []flagDef{
		workspaceFlag, {"--project", true}, {"--revision", true}, {"--confirm", false}, jsonFlag,
	}},
	"status": {flags: []flagDef{
		workspaceFlag, {"--root", true}, jsonFlag,
	}},
	"adopt": {
		flags: []flagDef{
			workspaceFlag, {"--root", true}, {"--apply", false}, {"--revision", true}, {"--fingerprint", true}, jsonFlag,
		},
		positional:    posAdoptPath,
		maxPositional: 1,
	},
	"detach": {
		flags: []flagDef{
			workspaceFlag, {"--root", true}, {"--apply", false}, {"--revision", true}, {"--fingerprint", true}, jsonFlag,
		},
		positional:    posDetachPath,
		maxPositional: 1,
	},
	"reconcile": {flags: []flagDef{
		workspaceFlag, {"--root", true}, {"--apply", false}, {"--revision", true}, {"--fingerprint", true}, jsonFlag,
	}},
	"migrate": {flags: []flagDef{
		workspaceFlag, {"--old-workspace", true}, {"--project", true}, {"--apply", false},
		{"--revision", true}, {"--fingerprint", true}, jsonFlag,
	}},
	"recover": {flags: []flagDef{
		workspaceFlag, {"--apply", false}, {"--discard", true}, {"--confirm", false}, jsonFlag,
	}},
	"mcp":        {flags: []flagDef{workspaceFlag}},
	"completion": {flags: []flagDef{{"--install", false}}, positional: posShell, maxPositional: 1},
	"help":       {},
	"version":    {},
}

// flagTakesValue reports whether flag consumes the following command-line
// word as its value (true) or is a boolean switch (false). It is derived
// from verbSpecs so there is exactly one place that has to know each flag's
// arity.
func flagTakesValue(flag string) bool {
	for _, spec := range verbSpecs {
		for _, def := range spec.flags {
			if def.name == flag {
				return def.takesValue
			}
		}
	}
	return false
}
