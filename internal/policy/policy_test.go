package policy_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/policy"
)

// fixture is the bytes of a contract fixture.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAbsentPolicyObservesEverything pins that no document means observe with an
// empty list, source none.
func TestAbsentPolicyObservesEverything(t *testing.T) {
	l := policy.None()
	if l.Source != "none" || l.Policy.Egress.Mode != policy.Observe || len(l.Policy.Egress.Allow) != 0 {
		t.Errorf("absent policy loaded as %+v", l)
	}
}

// TestFixturesReadWithDigest pins that every accepted fixture reads, with a digest and
// the mode it states, and that the digest is the document's, not the bytes': the same
// policy as YAML and as JSON has one digest.
func TestFixturesReadWithDigest(t *testing.T) {
	for name, mode := range map[string]policy.Mode{"observe.yaml": policy.Observe, "enforce.yaml": policy.Enforce, "enforce-nothing.yaml": policy.Enforce} {
		l, err := policy.Read(name, fixture(t, "fixtures/policy/"+name))
		if err != nil {
			t.Fatal(err)
		}
		if l.Source != "config" || len(l.Digest) != 64 || l.Policy.Egress.Mode != mode {
			t.Errorf("%s: read as %+v", name, l)
		}
	}
	yaml, err := policy.Read("enforce.yaml", fixture(t, "fixtures/policy/enforce.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := policy.Read("policy", []byte(`{"version":1,"egress":{"mode":"enforce","allow":["api.anthropic.com","*.github.com","github.com","registry.npmjs.org"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if yaml.Digest != js.Digest {
		t.Errorf("digests differ: %s %s", yaml.Digest, js.Digest)
	}
}

// TestInvalidPolicyIsAnError pins the rule that a configured policy that does not
// read means no run: a refused document is a *policy.Error naming the document.
func TestInvalidPolicyIsAnError(t *testing.T) {
	for _, name := range []string{"fixtures/invalid/policy-mode-log.yaml", "fixtures/invalid/policy-allow-widens.yaml"} {
		_, err := policy.Read(name, fixture(t, name))
		var pe *policy.Error
		if !errors.As(err, &pe) || pe.Name != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := policy.Read("policy", []byte(`{"version":1,"egress":{"mode":"log"}}`)); err == nil {
		t.Error("mode log was accepted")
	}
}

// TestMatchFollowsTheGrammar pins name, pattern and IP matching, and that a pattern
// never matches its apex.
func TestMatchFollowsTheGrammar(t *testing.T) {
	allow := []string{"api.anthropic.com", "*.github.com", "127.0.0.1"}
	for _, c := range []struct {
		host string
		rule string
		ok   bool
	}{
		{"api.anthropic.com", "api.anthropic.com", true},
		{"API.Anthropic.com.", "api.anthropic.com", true},
		{"anthropic.com", "", false},
		{"api.github.com", "*.github.com", true},
		{"a.b.github.com", "*.github.com", true},
		{"github.com", "", false},
		{"evilgithub.com", "", false},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.2", "", false},
	} {
		rule, ok := policy.Match(allow, c.host)
		if rule != c.rule || ok != c.ok {
			t.Errorf("Match(%q) = %q, %v; want %q, %v", c.host, rule, ok, c.rule, c.ok)
		}
	}
}

// TestCoversFollowsTheGrammar pins what stands above what: a name under the same name
// or a suffix above it, a pattern under the same pattern or a suffix above it, and a
// suffix never covers its own apex.
func TestCoversFollowsTheGrammar(t *testing.T) {
	for _, c := range []struct {
		entry, other string
		want         bool
	}{
		{"api.github.com", "api.github.com", true}, {"api.github.com", "API.github.com", true},
		{"*.github.com", "api.github.com", true}, {"*.github.com", "*.api.github.com", true},
		{"*.github.com", "github.com", false}, {"api.github.com", "*.github.com", false},
		{"*.github.com", "*.github.com", true}, {"github.com", "api.github.com", false},
	} {
		if got := policy.Covers(c.entry, c.other); got != c.want {
			t.Errorf("Covers(%q, %q) = %v", c.entry, c.other, got)
		}
	}
}
