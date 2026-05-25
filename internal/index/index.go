// Package index maintains backup/index.json, a JID-keyed snapshot of
// per-group ingestion progress. Downstream tools (summarizers, reporting
// agents) read it to find out which groups had new activity since their
// last run without having to scan every messages.jsonl on disk.
//
// Updates are debounced: every successful append updates an in-memory
// state under a short-lived mutex; the file itself is rewritten on a
// timer (RunPeriodic) and on shutdown (Flush). Writes are atomic — we
// serialize to "{root}/.index.json.tmp" first and then rename.
package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SchemaVersion is bumped when the on-disk layout changes in a way that
// is not backwards compatible with older readers.
const SchemaVersion = 1

// FileName is the canonical filename inside the backup root.
const FileName = "index.json"

// Entry is the per-group record persisted in the index. Field names
// match what a downstream agent will key off; do not rename without
// bumping SchemaVersion.
type Entry struct {
	Label          string    `json:"label"`
	Slug           string    `json:"slug,omitempty"`
	FirstMessageTS time.Time `json:"first_message_ts"`
	LastMessageTS  time.Time `json:"last_message_ts"`
	LastMessageID  string    `json:"last_message_id"`
	MessageCount   int       `json:"message_count"`
	LatestMonth    string    `json:"latest_month"`
}

// Snapshot is the serialized form of the index file.
type Snapshot struct {
	Schema    int              `json:"schema"`
	UpdatedAt time.Time        `json:"updated_at"`
	Groups    map[string]Entry `json:"groups"`
}

// Manager owns the in-memory state and is safe for concurrent use.
type Manager struct {
	root string
	log  *zap.SugaredLogger

	mu    sync.Mutex
	state map[string]Entry // keyed by JID
	dirty bool
}

// NewManager loads any pre-existing index.json so a restart preserves
// counts and timestamps. A missing or unreadable file results in an
// empty in-memory state (the index will be rebuilt as messages flow in).
func NewManager(root string, log *zap.SugaredLogger) *Manager {
	m := &Manager{
		root:  root,
		log:   log,
		state: make(map[string]Entry),
	}
	m.loadExisting()
	return m
}

func (m *Manager) loadExisting() {
	path := filepath.Join(m.root, FileName)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var snap Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		m.log.Warnw("could not parse existing index, starting fresh", "path", path, "error", err)
		return
	}
	if snap.Groups != nil {
		m.state = snap.Groups
	}
}

// Record updates the in-memory entry for one ingested message. It is
// idempotent: calling it twice with the same (jid, ts, id) results in
// double counting, so callers should only invoke it after a successful
// JSONL append.
func (m *Manager) Record(jid, label, slug, monthKey, messageID string, ts time.Time) {
	if jid == "" {
		return
	}
	tsUTC := ts.UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.state[jid]
	if !ok {
		e = Entry{FirstMessageTS: tsUTC}
	}
	if label != "" {
		e.Label = label
	}
	if slug != "" {
		e.Slug = slug
	}
	if e.FirstMessageTS.IsZero() || tsUTC.Before(e.FirstMessageTS) {
		e.FirstMessageTS = tsUTC
	}
	if tsUTC.After(e.LastMessageTS) {
		e.LastMessageTS = tsUTC
		e.LastMessageID = messageID
		e.LatestMonth = monthKey
	}
	e.MessageCount++
	m.state[jid] = e
	m.dirty = true
}

// Flush writes the index to disk if anything changed since the last
// flush. The write is atomic via tmp + rename. Returns nil when nothing
// was dirty.
func (m *Manager) Flush() error {
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	snap := Snapshot{
		Schema:    SchemaVersion,
		UpdatedAt: time.Now().UTC(),
		Groups:    cloneEntries(m.state),
	}
	m.dirty = false
	m.mu.Unlock()

	tmp := filepath.Join(m.root, "."+FileName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create tmp index: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&snap); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode index: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp index: %w", err)
	}
	final := filepath.Join(m.root, FileName)
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	return nil
}

// RunPeriodic flushes the index every interval until ctx is canceled.
// Callers should still call Flush() one last time after canceling ctx so
// any messages received between the last tick and shutdown are captured.
func (m *Manager) RunPeriodic(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Flush(); err != nil {
				m.log.Warnw("periodic index flush failed", "error", err)
			}
		}
	}
}

// Snapshot returns a defensive copy of the current state. Mostly useful
// for tests and for the shutdown summary.
func (m *Manager) SnapshotCopy() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		Schema:    SchemaVersion,
		UpdatedAt: time.Now().UTC(),
		Groups:    cloneEntries(m.state),
	}
}

func cloneEntries(in map[string]Entry) map[string]Entry {
	out := make(map[string]Entry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
