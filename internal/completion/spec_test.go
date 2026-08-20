package completion

import "testing"

func TestVerbsIncludesConfigAndInit(t *testing.T) {
	for _, want := range []string{"config", "init"} {
		found := false
		for _, verb := range Verbs {
			if verb == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Verbs = %v, missing %q", Verbs, want)
		}
	}
}

func TestVerbSpecsHasEntryForEveryVerb(t *testing.T) {
	for _, verb := range Verbs {
		if _, ok := verbSpecs[verb]; !ok {
			t.Errorf("verbSpecs is missing an entry for verb %q", verb)
		}
	}
}

func TestConfigSpecHasWorkspaceAndJSONFlags(t *testing.T) {
	spec, ok := verbSpecs["config"]
	if !ok {
		t.Fatal("verbSpecs[\"config\"] missing")
	}
	assertHasFlag(t, spec, "--workspace", true)
	assertHasFlag(t, spec, "--json", false)
}

func TestInitSpecHasWorkspaceAndFromGitFlags(t *testing.T) {
	spec, ok := verbSpecs["init"]
	if !ok {
		t.Fatal("verbSpecs[\"init\"] missing")
	}
	assertHasFlag(t, spec, "--workspace", true)
	assertHasFlag(t, spec, "--from-git", true)
}

// TestEveryStoreBuildingVerbAcceptsWorkspaceFlag mirrors the CLI's own
// rule (see cmd/ewasd's run()): every subcommand that builds a store
// accepts the global --workspace flag except completion/help/version.
func TestEveryStoreBuildingVerbAcceptsWorkspaceFlag(t *testing.T) {
	excluded := map[string]bool{"completion": true, "help": true, "version": true}
	for _, verb := range Verbs {
		if excluded[verb] {
			continue
		}
		spec := verbSpecs[verb]
		assertHasFlag(t, spec, "--workspace", true)
	}
}

func TestExcludedVerbsDoNotAdvertiseWorkspaceFlag(t *testing.T) {
	for _, verb := range []string{"completion", "help", "version"} {
		spec := verbSpecs[verb]
		for _, def := range spec.flags {
			if def.name == "--workspace" {
				t.Errorf("verb %q should not advertise --workspace", verb)
			}
		}
	}
}

func TestFlagTakesValueForWorkspace(t *testing.T) {
	if !flagTakesValue("--workspace") {
		t.Fatal("--workspace should take a value")
	}
}

func TestFlagTakesValueForFromGit(t *testing.T) {
	if !flagTakesValue("--from-git") {
		t.Fatal("--from-git should take a value")
	}
}

func assertHasFlag(t *testing.T, spec verbSpec, name string, takesValue bool) {
	t.Helper()
	for _, def := range spec.flags {
		if def.name == name {
			if def.takesValue != takesValue {
				t.Errorf("flag %q takesValue = %v, want %v", name, def.takesValue, takesValue)
			}
			return
		}
	}
	t.Errorf("spec is missing flag %q", name)
}
