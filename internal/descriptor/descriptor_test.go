package descriptor_test

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/descriptor"
)

// TestAnExpressionIsRefused pins that a descriptor matches and copies and never
// computes: the schema refuses a document that tries.
func TestAnExpressionIsRefused(t *testing.T) {
	bad, _ := fs.ReadFile(contracts.FS, "fixtures/invalid/descriptor-expression.yaml")
	if _, err := descriptor.Parse("descriptor.yaml", bad); err == nil {
		t.Error("a descriptor with an expression was accepted")
	}
}

// TestMatchIsEqualityOrPresenceOnly pins the match grammar: equality on strings,
// numbers and booleans across YAML and JSON decodings, presence with {present: true},
// nested paths, and no match on a missing path.
func TestMatchIsEqualityOrPresenceOnly(t *testing.T) {
	d := &descriptor.Descriptor{Rules: []descriptor.Rule{
		{Source: "output", Match: map[string]any{"type": "result", "num_turns": 1, "ok": true, "meta.kind": map[string]any{"present": true}}, Type: "dev.qory.session.result", Data: map[string]string{"n": "num_turns", "k": "meta.kind", "missing": "nope"}},
	}}
	rec := func(js string) descriptor.Record {
		var m map[string]any
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return descriptor.Record{Source: "output", Record: m}
	}
	typ, data, ok := d.Map(rec(`{"type":"result","num_turns":1,"ok":true,"meta":{"kind":"x"}}`))
	if !ok || typ != "dev.qory.session.result" || data["n"] != 1.0 || data["k"] != "x" {
		t.Errorf("Map = %s %v %v", typ, data, ok)
	}
	if _, hasMissing := data["missing"]; hasMissing {
		t.Error("a missing path produced a field")
	}
	for _, js := range []string{
		`{"type":"result","num_turns":2,"ok":true,"meta":{"kind":"x"}}`,
		`{"type":"result","num_turns":1,"ok":false,"meta":{"kind":"x"}}`,
		`{"type":"result","num_turns":1,"ok":true,"meta":{}}`,
		`{"type":"result","num_turns":"1","ok":true,"meta":{"kind":"x"}}`,
	} {
		if _, _, ok := d.Map(rec(js)); ok {
			t.Errorf("%s matched", js)
		}
	}
	if _, _, ok := d.Map(descriptor.Record{Source: "hooks", Record: map[string]any{"type": "result"}}); ok {
		t.Error("a rule matched a record of another source")
	}
}
