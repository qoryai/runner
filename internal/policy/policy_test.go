package policy_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/policy"
)

// write copies a contract fixture to a temporary file and returns its path.
func write(t *testing.T, fixture string) string {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, fixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAbsentPolicyObservesEverything pins that no path means observe with an empty
// list, source none.
func TestAbsentPolicyObservesEverything(t *testing.T) {
	l, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if l.Source != "none" || l.Policy.Egress.Mode != policy.Observe || len(l.Policy.Egress.Allow) != 0 {
		t.Errorf("absent policy loaded as %+v", l)
	}
}

// TestFixturesLoadWithDigest pins that every accepted fixture loads, with the file's
// digest and the mode it states.
func TestFixturesLoadWithDigest(t *testing.T) {
	for name, mode := range map[string]policy.Mode{"observe.yaml": policy.Observe, "enforce.yaml": policy.Enforce, "enforce-nothing.yaml": policy.Enforce} {
		l, err := policy.Load(write(t, "fixtures/policy/"+name))
		if err != nil {
			t.Fatal(err)
		}
		if l.Source != "file" || len(l.Digest) != 64 || l.Policy.Egress.Mode != mode {
			t.Errorf("%s: loaded as %+v", name, l)
		}
	}
}

// TestUnreadableOrInvalidPolicyIsAnError pins the rule that a configured policy that
// cannot be read means no run: a missing file and a refused document are both a
// *policy.Error naming the path.
func TestUnreadableOrInvalidPolicyIsAnError(t *testing.T) {
	for _, p := range []string{filepath.Join(t.TempDir(), "missing.yaml"), write(t, "fixtures/invalid/policy-mode-log.yaml"), write(t, "fixtures/invalid/policy-allow-widens.yaml")} {
		_, err := policy.Load(p)
		var pe *policy.Error
		if !errors.As(err, &pe) || pe.Path != p {
			t.Errorf("%s: %v", p, err)
		}
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

// TestNarrowKeepsOnlyWhatThePolicyCovers pins the intersection: declared entries the
// policy covers survive in declared order, the rest are dropped, and with no
// declaration the policy's own list is the effective one.
func TestNarrowKeepsOnlyWhatThePolicyCovers(t *testing.T) {
	l := &policy.Loaded{Policy: policy.Policy{Egress: policy.Egress{Mode: policy.Enforce, Allow: []string{"api.anthropic.com", "*.github.com"}}}}
	got := l.Narrow([]string{"registry.npmjs.org", "api.github.com", "*.api.github.com", "github.com", "api.anthropic.com", "api.github.com", "*.github.com"})
	want := []string{"api.github.com", "*.api.github.com", "api.anthropic.com", "*.github.com"}
	if len(got) != len(want) {
		t.Fatalf("Narrow = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Narrow = %v, want %v", got, want)
		}
	}
	if got := l.Narrow(nil); len(got) != 2 {
		t.Errorf("no declaration: %v", got)
	}
	if got := l.Narrow([]string{}); len(got) != 0 {
		t.Errorf("empty declaration reaches nothing, got %v", got)
	}
}
