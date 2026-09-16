package descriptor_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/descriptor"
)

// lines decodes a JSON lines file of the contract with encoding/json, the way the
// runner decodes records.
func lines(t *testing.T, name string, into func([]byte)) {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) > 0 {
			into(s.Bytes())
		}
	}
}

// TestEveryDescriptorFixtureReplays is the descriptor test the contract promises: the
// records of every fixture case, through the runtime's rules, produce exactly the
// expected events, type and data, in order.
func TestEveryDescriptorFixtureReplays(t *testing.T) {
	runtimes, err := fs.ReadDir(contracts.FS, "runtimes")
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range runtimes {
		d, err := descriptor.Load(rt.Name(), "")
		if err != nil {
			t.Fatal(err)
		}
		cases, _ := fs.ReadDir(contracts.FS, path.Join("runtimes", rt.Name(), "fixtures"))
		for _, c := range cases {
			dir := path.Join("runtimes", rt.Name(), "fixtures", c.Name())
			t.Run(rt.Name()+"/"+c.Name(), func(t *testing.T) {
				var got []map[string]any
				lines(t, path.Join(dir, "records.jsonl"), func(b []byte) {
					var r descriptor.Record
					if err := json.Unmarshal(b, &r); err != nil {
						t.Fatal(err)
					}
					if typ, data, ok := d.Map(r); ok {
						got = append(got, map[string]any{"type": typ, "data": data})
					}
				})
				var want []map[string]any
				lines(t, path.Join(dir, "expected", "events.jsonl"), func(b []byte) {
					var e map[string]any
					if err := json.Unmarshal(b, &e); err != nil {
						t.Fatal(err)
					}
					want = append(want, e)
				})
				if len(got) != len(want) {
					t.Fatalf("produced %d events, want %d: %v", len(got), len(want), got)
				}
				for i := range want {
					if !reflect.DeepEqual(roundTrip(t, got[i]), want[i]) {
						t.Errorf("event %d:\n got %v\nwant %v", i, got[i], want[i])
					}
				}
			})
		}
	}
}

// roundTrip puts a produced event through JSON so its numbers compare as the fixture's
// do.
func roundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOverrideDirectoryWinsAndInvalidIsRefused pins where a descriptor comes from: a
// file named for the runtime in the override directory replaces the embedded one, a
// file the schema refuses is an error, and an unknown runtime is an error naming it.
func TestOverrideDirectoryWinsAndInvalidIsRefused(t *testing.T) {
	dir := t.TempDir()
	b, _ := fs.ReadFile(contracts.FS, "runtimes/claude/descriptor.yaml")
	override := bytes.Replace(b, []byte(`runtime_version: "2.1.273"`), []byte(`runtime_version: "9.9.9"`), 1)
	if err := os.WriteFile(filepath.Join(dir, "claude.yaml"), override, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := descriptor.Load("claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.RuntimeVersion != "9.9.9" {
		t.Errorf("override not read: %s", d.RuntimeVersion)
	}
	if d, err := descriptor.Load("claude", t.TempDir()); err != nil || d.RuntimeVersion != "2.1.273" {
		t.Errorf("embedded default: %v %v", d, err)
	}
	if _, err := descriptor.Load("nosuch", ""); err == nil {
		t.Error("unknown runtime loaded")
	}
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
		{Source: "output", Match: map[string]any{"type": "result", "num_turns": 1, "ok": true, "meta.kind": map[string]any{"present": true}}, Type: "ai.qory.session.result", Data: map[string]string{"n": "num_turns", "k": "meta.kind", "missing": "nope"}},
	}}
	rec := func(js string) descriptor.Record {
		var m map[string]any
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return descriptor.Record{Source: "output", Record: m}
	}
	typ, data, ok := d.Map(rec(`{"type":"result","num_turns":1,"ok":true,"meta":{"kind":"x"}}`))
	if !ok || typ != "ai.qory.session.result" || data["n"] != 1.0 || data["k"] != "x" {
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
