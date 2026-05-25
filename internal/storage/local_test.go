package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStorage_AppendAndDedupMedia(t *testing.T) {
	dir := t.TempDir()
	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}

	if err := ls.AppendMessage("familia", "2025-01", []byte(`{"id":"1"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ls.AppendMessage("familia", "2025-01", []byte(`{"id":"2"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}

	rel, err := ls.WriteMedia("familia", "2025-01", "abc123", "jpg", []byte("first"))
	if err != nil {
		t.Fatalf("write media: %v", err)
	}
	if !strings.HasSuffix(rel, "familia/2025-01/media/abc123.jpg") {
		t.Fatalf("unexpected path: %q", rel)
	}

	// Same hash from a different group/month should dedup and return
	// the original path, not write the bytes again.
	rel2, err := ls.WriteMedia("amigos", "2025-02", "abc123", "jpg", []byte("second"))
	if err != nil {
		t.Fatalf("write dedup: %v", err)
	}
	if rel2 != rel {
		t.Fatalf("dedup should reuse path: got %q vs %q", rel2, rel)
	}

	if _, err := os.Stat(filepath.Join(dir, "amigos", "2025-02", "media", "abc123.jpg")); err == nil {
		t.Fatal("dedup'd file should NOT exist in the second group")
	}

	exists, p, err := ls.Exists("abc123", "jpg")
	if err != nil || !exists {
		t.Fatalf("Exists(abc123) = (%v, %q, %v)", exists, p, err)
	}
	if p != rel {
		t.Fatalf("Exists path mismatch: got %q want %q", p, rel)
	}

	// Close and reopen — the rehydration walk must find the existing media.
	if err := ls.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ls2, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ls2.Close()

	exists, p, err = ls2.Exists("abc123", "jpg")
	if err != nil || !exists {
		t.Fatalf("after reopen Exists(abc123) = (%v, %q, %v)", exists, p, err)
	}
	if p != rel {
		t.Fatalf("after reopen path mismatch: got %q want %q", p, rel)
	}

	// JSONL must contain both lines, one per row, with trailing newline.
	data, err := os.ReadFile(filepath.Join(dir, "familia", "2025-01", "messages.jsonl"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	got := string(data)
	want := `{"id":"1"}` + "\n" + `{"id":"2"}` + "\n"
	if got != want {
		t.Fatalf("jsonl contents mismatch:\ngot=%q\nwant=%q", got, want)
	}

	// Permission sanity: files 0600, dirs 0700. Skipped silently if the
	// underlying filesystem doesn't honour POSIX modes (FAT, exFAT, …).
	assertMode(t, filepath.Join(dir, "familia", "2025-01", "messages.jsonl"), 0o600)
	assertMode(t, filepath.Join(dir, "familia", "2025-01", "media", "abc123.jpg"), 0o600)
	assertMode(t, filepath.Join(dir, "familia"), 0o700)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %o, want %o", path, got, want)
	}
}
