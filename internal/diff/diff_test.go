package diff

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AylanBoscarino/wa-backup/internal/index"
)

func TestRun_EverythingNewWhenNoCursor(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, map[string]index.Entry{
		"a@g.us": {
			Label:         "A",
			LastMessageTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			LastMessageID: "M3",
			MessageCount:  3,
			Months:        []string{"2026-05"},
			LatestMonth:   "2026-05",
		},
	})
	writeJSONL(t, root, "a@g.us/2026-05/messages.jsonl",
		`{"id":"M1","timestamp":"2026-05-01T10:00:00Z","group_jid":"a@g.us","type":"text","text":"hi"}`,
		`{"id":"M2","timestamp":"2026-05-01T11:00:00Z","group_jid":"a@g.us","type":"text","text":"hello"}`,
		`{"id":"M3","timestamp":"2026-05-01T12:00:00Z","group_jid":"a@g.us","type":"text","text":"yo"}`,
	)

	var out bytes.Buffer
	if err := Run(Options{BackupRoot: root, CursorPath: filepath.Join(root, "nope.json")}, &out); err != nil {
		t.Fatal(err)
	}
	lines := splitLines(out.String())
	if len(lines) != 3 {
		t.Fatalf("expected 3 messages, got %d: %v", len(lines), lines)
	}
}

func TestRun_OnlyMessagesAfterCursor(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, map[string]index.Entry{
		"a@g.us": {
			LastMessageTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			LastMessageID: "M3",
			MessageCount:  3,
			Months:        []string{"2026-04", "2026-05"},
			LatestMonth:   "2026-05",
		},
	})
	writeJSONL(t, root, "a@g.us/2026-04/messages.jsonl",
		`{"id":"M0","timestamp":"2026-04-30T23:00:00Z","group_jid":"a@g.us","type":"text","text":"old"}`,
	)
	writeJSONL(t, root, "a@g.us/2026-05/messages.jsonl",
		`{"id":"M1","timestamp":"2026-05-01T10:00:00Z","group_jid":"a@g.us","type":"text","text":"yesterday"}`,
		`{"id":"M2","timestamp":"2026-05-01T11:00:00Z","group_jid":"a@g.us","type":"text","text":"new1"}`,
		`{"id":"M3","timestamp":"2026-05-01T12:00:00Z","group_jid":"a@g.us","type":"text","text":"new2"}`,
	)

	cursorPath := filepath.Join(root, "cursor.json")
	writeCursor(t, cursorPath, Cursor{Groups: map[string]GroupCursor{
		"a@g.us": {
			LastSummarizedTS: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
			LastSummarizedID: "M1",
		},
	}})

	var out bytes.Buffer
	if err := Run(Options{BackupRoot: root, CursorPath: cursorPath}, &out); err != nil {
		t.Fatal(err)
	}
	got := splitLines(out.String())
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if !strings.Contains(got[0], `"id":"M2"`) || !strings.Contains(got[1], `"id":"M3"`) {
		t.Errorf("unexpected lines:\n%s", strings.Join(got, "\n"))
	}
}

func TestRun_TupleCompareBreaksTimestampTies(t *testing.T) {
	// Two messages at the SAME timestamp; cursor is on the first one.
	// We must emit only the second one — neither lose it nor duplicate it.
	root := t.TempDir()
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	writeIndex(t, root, map[string]index.Entry{
		"a@g.us": {
			LastMessageTS: ts,
			LastMessageID: "MSG_B", // later in lex order
			Months:        []string{"2026-05"},
			LatestMonth:   "2026-05",
		},
	})
	writeJSONL(t, root, "a@g.us/2026-05/messages.jsonl",
		`{"id":"MSG_A","timestamp":"2026-05-01T12:00:00Z","group_jid":"a@g.us","type":"text","text":"first of pair"}`,
		`{"id":"MSG_B","timestamp":"2026-05-01T12:00:00Z","group_jid":"a@g.us","type":"text","text":"second of pair"}`,
	)
	cursorPath := filepath.Join(root, "cursor.json")
	writeCursor(t, cursorPath, Cursor{Groups: map[string]GroupCursor{
		"a@g.us": {LastSummarizedTS: ts, LastSummarizedID: "MSG_A"},
	}})

	var out bytes.Buffer
	if err := Run(Options{BackupRoot: root, CursorPath: cursorPath}, &out); err != nil {
		t.Fatal(err)
	}
	got := splitLines(out.String())
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"id":"MSG_B"`) {
		t.Errorf("expected MSG_B, got: %s", got[0])
	}
}

func TestRun_JIDFilter(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, map[string]index.Entry{
		"a@g.us": {
			LastMessageTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			Months:        []string{"2026-05"}, LatestMonth: "2026-05",
		},
		"b@g.us": {
			LastMessageTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			Months:        []string{"2026-05"}, LatestMonth: "2026-05",
		},
	})
	writeJSONL(t, root, "a@g.us/2026-05/messages.jsonl",
		`{"id":"A1","timestamp":"2026-05-01T10:00:00Z","group_jid":"a@g.us","type":"text","text":"from a"}`,
	)
	writeJSONL(t, root, "b@g.us/2026-05/messages.jsonl",
		`{"id":"B1","timestamp":"2026-05-01T10:00:00Z","group_jid":"b@g.us","type":"text","text":"from b"}`,
	)

	var out bytes.Buffer
	if err := Run(Options{BackupRoot: root, JIDFilter: "b@g.us"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "from a") || !strings.Contains(got, "from b") {
		t.Errorf("filter failed:\n%s", got)
	}
}

func TestRun_NoNewMessagesIsEmptyOutput(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, map[string]index.Entry{
		"a@g.us": {
			LastMessageTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			LastMessageID: "M1",
			Months:        []string{"2026-05"}, LatestMonth: "2026-05",
		},
	})
	writeJSONL(t, root, "a@g.us/2026-05/messages.jsonl",
		`{"id":"M1","timestamp":"2026-05-01T12:00:00Z","group_jid":"a@g.us","type":"text","text":"hi"}`,
	)
	cursorPath := filepath.Join(root, "cursor.json")
	writeCursor(t, cursorPath, Cursor{Groups: map[string]GroupCursor{
		"a@g.us": {
			LastSummarizedTS: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			LastSummarizedID: "M1",
		},
	}})

	var out bytes.Buffer
	if err := Run(Options{BackupRoot: root, CursorPath: cursorPath}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected empty output, got: %q", out.String())
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func writeIndex(t *testing.T, root string, groups map[string]index.Entry) {
	t.Helper()
	snap := index.Snapshot{Schema: 1, UpdatedAt: time.Now().UTC(), Groups: groups}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCursor(t *testing.T, path string, c Cursor) {
	t.Helper()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONL(t *testing.T, root, rel string, lines ...string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
