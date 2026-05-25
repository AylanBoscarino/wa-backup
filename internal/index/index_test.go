package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func nop() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestRecordAndFlush(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nop())

	jid := "120363012345678901@g.us"
	m.Record(jid, "Família Doe", "famlia-doe", "2025-01", "MSG1", time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC))
	m.Record(jid, "Família Doe", "famlia-doe", "2025-01", "MSG2", time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC))
	m.Record(jid, "Família Doe", "famlia-doe", "2025-02", "MSG3", time.Date(2025, 2, 3, 9, 0, 0, 0, time.UTC))

	if err := m.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Schema != SchemaVersion {
		t.Errorf("schema = %d", snap.Schema)
	}
	e, ok := snap.Groups[jid]
	if !ok {
		t.Fatalf("missing group: %#v", snap.Groups)
	}
	if e.MessageCount != 3 {
		t.Errorf("count = %d, want 3", e.MessageCount)
	}
	if e.LastMessageID != "MSG3" {
		t.Errorf("last_id = %q, want MSG3", e.LastMessageID)
	}
	if e.LatestMonth != "2025-02" {
		t.Errorf("latest_month = %q, want 2025-02", e.LatestMonth)
	}
	if !e.FirstMessageTS.Equal(time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first = %v", e.FirstMessageTS)
	}
	if !e.LastMessageTS.Equal(time.Date(2025, 2, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last = %v", e.LastMessageTS)
	}

	// File mode is 0600 (we open it that way; permissions verified loosely).
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFlushNoOpWhenClean(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nop())
	if err := m.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Errorf("clean flush should not create file; got err=%v", err)
	}
}

func TestPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nop())
	m.Record("x@g.us", "X", "x", "2026-05", "ID1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, nop())
	snap := m2.SnapshotCopy()
	e, ok := snap.Groups["x@g.us"]
	if !ok {
		t.Fatalf("restart lost state: %#v", snap.Groups)
	}
	if e.MessageCount != 1 || e.LastMessageID != "ID1" {
		t.Errorf("e = %#v", e)
	}

	// New record advances state.
	m2.Record("x@g.us", "X", "x", "2026-05", "ID2", time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
	if err := m2.Flush(); err != nil {
		t.Fatal(err)
	}

	m3 := NewManager(dir, nop())
	e = m3.SnapshotCopy().Groups["x@g.us"]
	if e.MessageCount != 2 || e.LastMessageID != "ID2" {
		t.Errorf("after second restart e = %#v", e)
	}
}

func TestLabelAndSlugRenameOnlyAffectLabels(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nop())
	jid := "abc@g.us"
	m.Record(jid, "Original Name", "original-name", "2026-01", "ID1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m.Record(jid, "Renamed", "renamed", "2026-02", "ID2", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))

	e := m.SnapshotCopy().Groups[jid]
	if e.Label != "Renamed" {
		t.Errorf("label = %q, want Renamed (most-recent wins)", e.Label)
	}
	if e.MessageCount != 2 {
		t.Errorf("rename must not reset count: %d", e.MessageCount)
	}
	if e.FirstMessageTS.IsZero() || !e.FirstMessageTS.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first_ts should still be the earliest = %v", e.FirstMessageTS)
	}
}
