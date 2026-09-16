package chunk_test

import (
	"strings"
	"testing"

	"github.com/qoryai/runner/internal/chunk"
)

// TestChunksCutAtLinesAndAtSize pins the rule: a newline ends a chunk and keeps its
// newline, a long line is cut at Size, and a partial line waits for Flush.
func TestChunksCutAtLinesAndAtSize(t *testing.T) {
	var got []string
	w := chunk.New(func(b []byte) { got = append(got, string(b)) })
	long := strings.Repeat("x", chunk.Size+10)
	w.Write([]byte("one\ntwo\nthr"))
	w.Write([]byte("ee\n" + long + "\ntail"))
	if len(got) != 5 {
		t.Fatalf("%d chunks: %q", len(got), got)
	}
	want := []string{"one\n", "two\n", "three\n", long[:chunk.Size], long[chunk.Size:] + "\n"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, got[i], want[i])
		}
	}
	w.Flush()
	if len(got) != 6 || got[5] != "tail" {
		t.Errorf("after flush: %q", got[len(got)-1])
	}
	w.Flush()
	if len(got) != 6 {
		t.Error("a second flush emitted something")
	}
	if strings.Join(got, "") != "one\ntwo\nthree\n"+long+"\ntail" {
		t.Error("the chunks do not concatenate to the input")
	}
}
