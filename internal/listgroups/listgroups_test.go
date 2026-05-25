package listgroups

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSortRows_Recent(t *testing.T) {
	now := time.Now()
	rows := []Row{
		{Name: "alpha", LastMsg: now.Add(-1 * time.Hour), Created: now.Add(-72 * time.Hour)},
		{Name: "bravo", LastMsg: time.Time{}, Created: now.Add(-24 * time.Hour)},
		{Name: "charlie", LastMsg: now.Add(-10 * time.Minute), Created: now.Add(-48 * time.Hour)},
		{Name: "delta", LastMsg: time.Time{}, Created: now.Add(-12 * time.Hour)},
	}
	sortRows(rows, SortRecent, false)
	want := []string{"charlie", "alpha", "delta", "bravo"}
	for i, r := range rows {
		if r.Name != want[i] {
			t.Errorf("position %d: got %q, want %q (full order: %v)", i, r.Name, want[i], names(rows))
		}
	}
}

func TestSortRows_NameAndReverse(t *testing.T) {
	rows := []Row{{Name: "Beta"}, {Name: "alpha"}, {Name: "Gamma"}}
	sortRows(rows, SortName, false)
	if got := names(rows); !equal(got, []string{"alpha", "Beta", "Gamma"}) {
		t.Errorf("name sort: %v", got)
	}
	sortRows(rows, SortName, true)
	if got := names(rows); !equal(got, []string{"Gamma", "Beta", "alpha"}) {
		t.Errorf("reverse name sort: %v", got)
	}
}

func TestParseSortKey(t *testing.T) {
	for _, ok := range []string{"recent", "NAME", "Members", " created "} {
		if _, err := ParseSortKey(ok); err != nil {
			t.Errorf("%q should parse, got %v", ok, err)
		}
	}
	if _, err := ParseSortKey("nope"); err == nil {
		t.Error("expected error for invalid sort key")
	}
}

func TestLoadLastMessageByJID(t *testing.T) {
	root := t.TempDir()
	// Two months for the same group; the later month wins.
	mustWrite(t, root, "familia/2025-01/messages.jsonl",
		`{"id":"1","timestamp":"2025-01-15T10:00:00Z","group_jid":"abc@g.us"}`+"\n",
	)
	mustWrite(t, root, "familia/2025-02/messages.jsonl",
		`{"id":"2","timestamp":"2025-02-03T12:00:00Z","group_jid":"abc@g.us"}`+"\n"+
			`{"id":"3","timestamp":"2025-02-20T15:30:00Z","group_jid":"abc@g.us"}`+"\n",
	)
	// Different group, only earlier month.
	mustWrite(t, root, "amigos/2025-01/messages.jsonl",
		`{"id":"4","timestamp":"2025-01-05T08:00:00Z","group_jid":"xyz@g.us"}`+"\n",
	)
	// Junk dir without month subdirs is ignored.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := loadLastMessageByJID(root)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d (%v)", len(got), got)
	}
	if !got["abc@g.us"].Equal(time.Date(2025, 2, 20, 15, 30, 0, 0, time.UTC)) {
		t.Errorf("abc last_msg = %v", got["abc@g.us"])
	}
	if !got["xyz@g.us"].Equal(time.Date(2025, 1, 5, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("xyz last_msg = %v", got["xyz@g.us"])
	}
}

func TestWriteTable_NoHeader(t *testing.T) {
	var buf bytes.Buffer
	rows := []Row{{JID: "abc@g.us", Name: "Demo", Members: 3}}
	if err := writeTable(&buf, rows, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "JID") {
		t.Errorf("no-header should suppress header, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "abc@g.us") {
		t.Errorf("expected row in output: %q", buf.String())
	}
}

func mustWrite(t *testing.T, root, rel, contents string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
