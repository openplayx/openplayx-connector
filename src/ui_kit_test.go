package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestUIKitMatchesManifest keeps web/ui an unmodified copy of the shared
// OpenPlayX UI kit. Edit openplayx/web ui/ and run `node scripts/sync-ui.mjs --all`.
func TestUIKitMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("web", "ui", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Files) == 0 {
		t.Fatalf("invalid manifest: %v", err)
	}
	for name, want := range manifest.Files {
		content, err := os.ReadFile(filepath.Join("web", "ui", filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s differs from the shared UI kit; sync it from openplayx/web instead of editing it here", name)
		}
	}
}
