package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MigrateLegacyDirs walks the backup root and renames pre-JID slug
// directories (e.g. "erik--aylan") to JID-based directories
// (e.g. "120363428945290436@g.us"). It is safe to run repeatedly:
// directories that already look like a JID are skipped, and so are any
// dirs without a parseable messages.jsonl. Returns the number of
// directories migrated and the first non-fatal error encountered, if any.
//
// Migration strategy:
//  1. Read first valid JSONL line under {dir}/YYYY-MM/messages.jsonl.
//  2. Extract the "group_jid" field to learn the canonical key.
//  3. os.Rename the legacy directory to {root}/{group_jid}.
//
// If a JID-named directory already exists alongside a legacy one,
// migration of that legacy dir is skipped with a warning; merging is
// left to the operator because it requires choosing how to combine
// overlapping monthly files.
func MigrateLegacyDirs(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var migrated int
	var firstWarn error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Hidden files (e.g. the future .index.json.tmp during writes)
		// stay alone. Already-migrated dirs do too.
		if strings.HasPrefix(name, ".") || isJIDDirName(name) {
			continue
		}
		legacyPath := filepath.Join(root, name)
		jid, err := detectGroupJID(legacyPath)
		if err != nil {
			if firstWarn == nil {
				firstWarn = fmt.Errorf("detect jid for %q: %w", name, err)
			}
			continue
		}
		newPath := filepath.Join(root, jid)
		if _, err := os.Stat(newPath); err == nil {
			if firstWarn == nil {
				firstWarn = fmt.Errorf("cannot migrate %q: target %q already exists; merge manually", name, jid)
			}
			continue
		}
		if err := os.Rename(legacyPath, newPath); err != nil {
			if firstWarn == nil {
				firstWarn = fmt.Errorf("rename %q -> %q: %w", name, jid, err)
			}
			continue
		}
		migrated++
	}
	return migrated, firstWarn
}

// isJIDDirName reports whether the directory name already follows the
// JID convention used by this version of the daemon. We accept both
// group JIDs (@g.us) and direct-message JIDs (@s.whatsapp.net) so the
// check still works if individual chats are ever stored here.
func isJIDDirName(name string) bool {
	return strings.HasSuffix(name, "@g.us") || strings.HasSuffix(name, "@s.whatsapp.net")
}

// detectGroupJID returns the WhatsApp group JID extracted from the
// first parseable line of any messages.jsonl under the directory.
func detectGroupJID(groupDir string) (string, error) {
	monthDirs, err := os.ReadDir(groupDir)
	if err != nil {
		return "", err
	}
	for _, m := range monthDirs {
		if !m.IsDir() {
			continue
		}
		jsonlPath := filepath.Join(groupDir, m.Name(), "messages.jsonl")
		jid, err := readFirstGroupJID(jsonlPath)
		if err == nil && jid != "" {
			return jid, nil
		}
	}
	return "", errors.New("no parseable group_jid in any month")
}

func readFirstGroupJID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// JSONL lines can be large (huge raw protobuf dumps for unknown
	// types). Allow up to 4 MiB per line before giving up.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var msg struct {
			GroupJID string `json:"group_jid"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err == nil && msg.GroupJID != "" {
			return msg.GroupJID, nil
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return "", errors.New("no group_jid found")
}
