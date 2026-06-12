// Package listgroups implements the --list-groups subcommand: connect once,
// pull the joined-groups list, enrich it with last-message timestamps from
// the local backup, and print as a table or JSON.
package listgroups

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AylanBoscarino/wa-backup/config"
	"github.com/AylanBoscarino/wa-backup/internal/client"
	"go.uber.org/zap"
)

// Options controls list-groups behavior. Defaults are set by main.go to
// match the documented CLI defaults.
type Options struct {
	Limit    int
	All      bool
	JSON     bool
	Search   string
	SortBy   SortKey
	Reverse  bool
	NoHeader bool
}

type SortKey string

const (
	SortRecent  SortKey = "recent"
	SortName    SortKey = "name"
	SortMembers SortKey = "members"
	SortCreated SortKey = "created"
)

func ParseSortKey(s string) (SortKey, error) {
	switch SortKey(strings.ToLower(strings.TrimSpace(s))) {
	case SortRecent:
		return SortRecent, nil
	case SortName:
		return SortName, nil
	case SortMembers:
		return SortMembers, nil
	case SortCreated:
		return SortCreated, nil
	}
	return "", fmt.Errorf("invalid --sort value %q (want recent|name|members|created)", s)
}

// Row is one entry in the listing. Times are zero when unknown.
type Row struct {
	JID     string    `json:"jid"`
	Name    string    `json:"name"`
	Members int       `json:"members"`
	LastMsg time.Time `json:"last_msg,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

// Run is the entrypoint. It blocks until the listing has been written
// to `out` or an error occurs.
func Run(ctx context.Context, cfg *config.Config, log *zap.SugaredLogger, opts Options, out io.Writer) error {
	waClient, err := client.New(ctx, cfg.SessionDBPath, log)
	if err != nil {
		return fmt.Errorf("init whatsapp client: %w", err)
	}
	cli := waClient.Underlying()
	if cli.Store.ID == nil {
		return fmt.Errorf("no session at %s — start the daemon at least once to pair your device", cfg.SessionDBPath)
	}

	// PairIfNeeded short-circuits to reconnectWithBackoff for already-
	// paired sessions, which waits for IsLoggedIn before returning.
	if err := waClient.PairIfNeeded(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer waClient.Shutdown()

	groups, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("get joined groups: %w", err)
	}
	log.Debugw("fetched joined groups", "count", len(groups))

	lastMsgByJID := loadLastMessageByJID(cfg.BackupPath)

	rows := make([]Row, 0, len(groups))
	for _, g := range groups {
		jid := g.JID.String()
		membersCount := g.ParticipantCount
		if membersCount == 0 && len(g.Participants) > 0 {
			membersCount = len(g.Participants)
		}
		rows = append(rows, Row{
			JID:     jid,
			Name:    g.Name,
			Members: membersCount,
			LastMsg: lastMsgByJID[jid],
			Created: g.GroupCreated,
		})
	}

	if opts.Search != "" {
		needle := strings.ToLower(opts.Search)
		filtered := rows[:0]
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Name), needle) {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	sortKey := opts.SortBy
	if sortKey == "" {
		sortKey = SortRecent
	}
	sortRows(rows, sortKey, opts.Reverse)

	if !opts.All && opts.Limit > 0 && len(rows) > opts.Limit {
		rows = rows[:opts.Limit]
	}

	if opts.JSON {
		return writeJSON(out, rows)
	}
	return writeTable(out, rows, opts.NoHeader)
}

// sortRows orders rows in-place. For "recent", groups without a backup
// timestamp fall to the bottom; ties are broken by GroupCreated desc, then
// name asc, so the output is stable.
func sortRows(rows []Row, key SortKey, reverse bool) {
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch key {
		case SortName:
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		case SortMembers:
			if a.Members != b.Members {
				return a.Members > b.Members
			}
		case SortCreated:
			if !a.Created.Equal(b.Created) {
				return a.Created.After(b.Created)
			}
		case SortRecent:
			fallthrough
		default:
			aZ, bZ := a.LastMsg.IsZero(), b.LastMsg.IsZero()
			switch {
			case !aZ && !bZ && !a.LastMsg.Equal(b.LastMsg):
				return a.LastMsg.After(b.LastMsg)
			case aZ != bZ:
				return !aZ // groups without backup go to the bottom
			default:
				if !a.Created.Equal(b.Created) {
					return a.Created.After(b.Created)
				}
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
	sort.SliceStable(rows, less)
	if reverse {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
}

func writeJSON(out io.Writer, rows []Row) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func writeTable(out io.Writer, rows []Row, noHeader bool) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if !noHeader {
		fmt.Fprintln(tw, "JID\tNAME\tMEMBERS\tLAST_MSG\tCREATED")
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			r.JID,
			r.Name,
			r.Members,
			formatTime(r.LastMsg),
			formatTime(r.Created),
		)
	}
	return tw.Flush()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// loadLastMessageByJID walks the backup root and returns the most-recent
// message timestamp per group JID. Reads only the tail of the latest
// monthly JSONL per group, so it scales with number-of-groups, not
// number-of-messages.
func loadLastMessageByJID(root string) map[string]time.Time {
	result := map[string]time.Time{}
	groupDirs, err := os.ReadDir(root)
	if err != nil {
		return result
	}
	for _, gd := range groupDirs {
		if !gd.IsDir() {
			continue
		}
		ts, jid := scanGroupLastMessage(filepath.Join(root, gd.Name()))
		if jid == "" || ts.IsZero() {
			continue
		}
		if existing, ok := result[jid]; !ok || ts.After(existing) {
			result[jid] = ts
		}
	}
	return result
}

func scanGroupLastMessage(groupDir string) (time.Time, string) {
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return time.Time{}, ""
	}
	var months []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if matched, _ := filepath.Match("????-??", e.Name()); matched {
			months = append(months, e.Name())
		}
	}
	if len(months) == 0 {
		return time.Time{}, ""
	}
	sort.Strings(months)
	for i := len(months) - 1; i >= 0; i-- {
		path := filepath.Join(groupDir, months[i], "messages.jsonl")
		if ts, jid := readLastLine(path); !ts.IsZero() {
			return ts, jid
		}
	}
	return time.Time{}, ""
}

// readLastLine reads the trailing chunk of a JSONL file and returns the
// timestamp + group_jid from the last non-empty line. Bounded buffer so
// huge files don't blow memory.
func readLastLine(path string) (time.Time, string) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return time.Time{}, ""
	}

	const tailBytes = 64 * 1024
	size := fi.Size()
	bufSize := int64(tailBytes)
	if size < bufSize {
		bufSize = size
	}
	buf := make([]byte, bufSize)
	if _, err := f.ReadAt(buf, size-bufSize); err != nil && err != io.EOF {
		return time.Time{}, ""
	}

	// Trim trailing newlines, then take everything after the last newline.
	end := len(buf)
	for end > 0 && (buf[end-1] == '\n' || buf[end-1] == '\r') {
		end--
	}
	start := bytes.LastIndexByte(buf[:end], '\n')
	if start < 0 {
		start = 0
	} else {
		start++
	}

	var msg struct {
		Timestamp time.Time `json:"timestamp"`
		GroupJID  string    `json:"group_jid"`
	}
	if err := json.Unmarshal(buf[start:end], &msg); err != nil {
		return time.Time{}, ""
	}
	return msg.Timestamp, msg.GroupJID
}
