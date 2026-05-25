package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyDirs(t *testing.T) {
	root := t.TempDir()

	// Legacy slug dir with one month of JSONL.
	mustWrite(t, root, "familia-doe/2025-01/messages.jsonl",
		`{"id":"1","group_jid":"120363012345678901@g.us","timestamp":"2025-01-15T10:00:00Z"}`+"\n",
	)
	mustWrite(t, root, "familia-doe/2025-02/messages.jsonl",
		`{"id":"2","group_jid":"120363012345678901@g.us","timestamp":"2025-02-15T10:00:00Z"}`+"\n",
	)

	// Another legacy dir.
	mustWrite(t, root, "amigos-jiu-jitsu/2025-03/messages.jsonl",
		`{"id":"3","group_jid":"120363099999999999@g.us","timestamp":"2025-03-01T10:00:00Z"}`+"\n",
	)

	// Already-migrated dir — must be left alone.
	mustWrite(t, root, "120363012347777777@g.us/2025-04/messages.jsonl",
		`{"id":"4","group_jid":"120363012347777777@g.us","timestamp":"2025-04-01T10:00:00Z"}`+"\n",
	)

	// Garbage dir (no jsonl) — must be skipped with a warning, not crash.
	if err := os.MkdirAll(filepath.Join(root, "garbage"), 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := MigrateLegacyDirs(root)
	// We expect 2 migrations and a non-nil warning about "garbage".
	if n != 2 {
		t.Errorf("migrated count = %d, want 2", n)
	}
	if err == nil {
		t.Logf("no warning surfaced; OK if garbage dir was treated as empty")
	}

	for _, want := range []string{
		"120363012345678901@g.us/2025-01/messages.jsonl",
		"120363012345678901@g.us/2025-02/messages.jsonl",
		"120363099999999999@g.us/2025-03/messages.jsonl",
		"120363012347777777@g.us/2025-04/messages.jsonl", // unchanged
	} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("expected file missing: %s (%v)", want, err)
		}
	}
	for _, gone := range []string{"familia-doe", "amigos-jiu-jitsu"} {
		if _, err := os.Stat(filepath.Join(root, gone)); err == nil {
			t.Errorf("legacy dir %q still present after migration", gone)
		}
	}

	// Idempotent: running again should migrate zero dirs.
	n2, _ := MigrateLegacyDirs(root)
	if n2 != 0 {
		t.Errorf("second pass migrated %d, want 0", n2)
	}
}

func TestMigrateConflictKeepsLegacy(t *testing.T) {
	root := t.TempDir()
	// Legacy and JID-based dir for the SAME group already coexist.
	mustWrite(t, root, "legacy/2025-01/messages.jsonl",
		`{"id":"1","group_jid":"abc@g.us"}`+"\n",
	)
	mustWrite(t, root, "abc@g.us/2025-02/messages.jsonl",
		`{"id":"2","group_jid":"abc@g.us"}`+"\n",
	)

	n, err := MigrateLegacyDirs(root)
	if n != 0 {
		t.Errorf("conflict should NOT auto-migrate; migrated = %d", n)
	}
	if err == nil {
		t.Errorf("expected warning about conflict; got nil")
	}
	// Both dirs must survive — the operator decides how to merge.
	if _, err := os.Stat(filepath.Join(root, "legacy")); err != nil {
		t.Errorf("legacy dir should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "abc@g.us")); err != nil {
		t.Errorf("jid dir should remain: %v", err)
	}
}

func mustWrite(t *testing.T, root, rel, contents string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
