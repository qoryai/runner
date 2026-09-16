package receiver_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/receiver"
)

// TestLoadWebhookReadsTheRunnersFile is one file for both ends: the receiver reads the
// same webhook configuration the runner posts with, and refuses to run without one.
func TestLoadWebhookReadsTheRunnersFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webhook.yaml")
	b, err := fs.ReadFile(contracts.FS, "fixtures/webhook/some-events.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := receiver.LoadWebhook(path)
	if err != nil {
		t.Fatal(err)
	}
	if w.URL == "" || w.Secret == "" || len(w.Events) == 0 {
		t.Errorf("loaded %+v", w)
	}
	if _, err := receiver.LoadWebhook(""); err == nil {
		t.Error("no path was accepted")
	}
	if _, err := receiver.LoadWebhook(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file was accepted")
	}
}
