package catalog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qoryai/runner/runtimes/catalog"
	"github.com/qoryai/runner/runtimes/runtimetest"
)

// TestLookupIsTheMachinesDescriptorThenTheContractsThenBare pins where a runtime comes
// from by its name.
func TestLookupIsTheMachinesDescriptorThenTheContractsThenBare(t *testing.T) {
	rt, err := catalog.Lookup("claude", "")
	if err != nil || rt.Version() == "" || !rt.ReadsOutput() {
		t.Fatalf("the contract's: %+v %v", rt, err)
	}
	runtimetest.Conforms(t, rt)

	rt, err = catalog.Lookup("codex", t.TempDir())
	if err != nil || rt.Name() != "codex" || rt.ReadsOutput() {
		t.Fatalf("a name nothing describes: %+v %v", rt, err)
	}

	dir := t.TempDir()
	doc := "version: 1\nruntime: codex\nruntime_version: \"9\"\nsources:\n  output: {format: jsonl}\nstop: {signal: SIGHUP}\nrules:\n  - {source: output, match: {type: end}, type: dev.qory.session.ended, data: {reason: why}}\n"
	if err := os.WriteFile(filepath.Join(dir, "codex.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err = catalog.Lookup("codex", dir)
	if err != nil || rt.Version() != "9" || rt.Stop().Signal != "SIGHUP" {
		t.Fatalf("the machine's: %+v %v", rt, err)
	}
	runtimetest.Conforms(t, rt)

	if err := os.WriteFile(filepath.Join(dir, "claude.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Lookup("claude", dir); err == nil || !strings.Contains(err.Error(), `describes "codex"`) {
		t.Errorf("a descriptor of another runtime: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "amp.yaml"), []byte("version: 1\nrules: compute\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Lookup("amp", dir); err == nil {
		t.Error("a descriptor the schema refuses was read")
	}
	for _, bad := range []string{"", "../claude", "Claude", "a b"} {
		if _, err := catalog.Lookup(bad, dir); err == nil {
			t.Errorf("%q is a name", bad)
		}
	}
}
