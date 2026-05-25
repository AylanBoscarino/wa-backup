// Package diff implements the --diff-since helper. Given a cursor file
// recording (last_summarized_ts, last_summarized_id) per group, it
// streams every JSONL message in the backup that is strictly newer than
// the cursor, in chronological order.
//
// The (ts, id) tuple is required because WhatsApp timestamps are only
// second-resolution, and grouping a strict > comparison on ts alone
// either drops messages that share a second or duplicates the cursor
// message itself, depending on which inequality is used. Comparing
// lexicographically on (ts, id) sidesteps the choice entirely.
package diff

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/AylanBoscarino/wa-backup/internal/index"
)

// Cursor is the agent-owned state file. The daemon never writes to it.
// Format is intentionally minimal and stable across schema bumps.
type Cursor struct {
	Groups map[string]GroupCursor `json:"groups"`
}

type GroupCursor struct {
	LastSummarizedTS time.Time `json:"last_summarized_ts"`
	LastSummarizedID string    `json:"last_summarized_id"`
}

// Options controls Run.
type Options struct {
	BackupRoot string // path to the backup directory containing index.json
	CursorPath string // path to the agent's cursor file (may not exist yet)
	JIDFilter  string // if non-empty, only emit messages from this group
}

// Run streams JSONL of new messages to out and returns when done. Does
// NOT update the cursor file — that is the agent's responsibility after
// it has successfully consumed the stream.
func Run(opts Options, out io.Writer) error {
	if opts.BackupRoot == "" {
		return errors.New("BackupRoot required")
	}
	indexPath := filepath.Join(opts.BackupRoot, index.FileName)
	snap, err := loadIndex(indexPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", indexPath, err)
	}
	cur, err := loadCursor(opts.CursorPath)
	if err != nil {
		return fmt.Errorf("read cursor %s: %w", opts.CursorPath, err)
	}

	jids := pickJIDs(snap, opts.JIDFilter)

	for _, jid := range jids {
		g := snap.Groups[jid]
		c := cur.Groups[jid]
		if !isAfter(g.LastMessageTS, g.LastMessageID, c.LastSummarizedTS, c.LastSummarizedID) {
			continue
		}
		months := g.Months
		if len(months) == 0 && g.LatestMonth != "" {
			months = []string{g.LatestMonth}
		}
		for _, month := range months {
			path := filepath.Join(opts.BackupRoot, jid, month, "messages.jsonl")
			if err := streamNewer(path, c.LastSummarizedTS, c.LastSummarizedID, out); err != nil {
				return fmt.Errorf("stream %s: %w", path, err)
			}
		}
	}
	return nil
}

func loadIndex(path string) (index.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return index.Snapshot{}, err
	}
	defer f.Close()
	var snap index.Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return index.Snapshot{}, err
	}
	if snap.Groups == nil {
		snap.Groups = map[string]index.Entry{}
	}
	return snap, nil
}

func loadCursor(path string) (Cursor, error) {
	if path == "" {
		return Cursor{Groups: map[string]GroupCursor{}}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Treat "no cursor yet" as "summarize from scratch".
			return Cursor{Groups: map[string]GroupCursor{}}, nil
		}
		return Cursor{}, err
	}
	defer f.Close()
	var c Cursor
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return Cursor{}, err
	}
	if c.Groups == nil {
		c.Groups = map[string]GroupCursor{}
	}
	return c, nil
}

// pickJIDs returns the group IDs to walk, in order of last_message_ts
// ascending so the output stays chronological across groups when piped
// straight to a consumer.
func pickJIDs(snap index.Snapshot, filter string) []string {
	jids := make([]string, 0, len(snap.Groups))
	for jid := range snap.Groups {
		if filter != "" && jid != filter {
			continue
		}
		jids = append(jids, jid)
	}
	sort.Slice(jids, func(i, j int) bool {
		ti := snap.Groups[jids[i]].LastMessageTS
		tj := snap.Groups[jids[j]].LastMessageTS
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return jids[i] < jids[j]
	})
	return jids
}

// isAfter reports (ts, id) > (cursorTS, cursorID) using lexicographic
// tuple compare. Zero cursor means "everything is new".
func isAfter(ts time.Time, id string, cursorTS time.Time, cursorID string) bool {
	if cursorTS.IsZero() {
		return true
	}
	if ts.After(cursorTS) {
		return true
	}
	if ts.Equal(cursorTS) {
		return id > cursorID
	}
	return false
}

// streamNewer parses each JSONL line and emits it verbatim if it is
// strictly after (cursorTS, cursorID). Bad lines are skipped silently
// so a single corruption doesn't kill the whole stream.
func streamNewer(path string, cursorTS time.Time, cursorID string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	w := bufio.NewWriter(out)
	defer w.Flush()

	for scanner.Scan() {
		line := scanner.Bytes()
		var head struct {
			ID        string    `json:"id"`
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue // skip malformed
		}
		if !isAfter(head.Timestamp, head.ID, cursorTS, cursorID) {
			continue
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
