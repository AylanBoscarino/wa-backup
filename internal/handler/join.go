package handler

import (
	"context"
	"os"
	"strings"

	"github.com/AylanBoscarino/wa-backup/internal/pipeline"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

// JoinHandler handles events.JoinedGroup to dynamically start backing up
// newly joined groups and persist their JIDs to the allowlist in the .env file.
type JoinHandler struct {
	filter *pipeline.GroupFilter
	log    *zap.SugaredLogger
}

func NewJoinHandler(f *pipeline.GroupFilter, log *zap.SugaredLogger) *JoinHandler {
	return &JoinHandler{filter: f, log: log}
}

func (h *JoinHandler) Handle(ctx context.Context, evt *events.JoinedGroup) {
	if evt == nil {
		return
	}
	jid := evt.JID.String()
	h.log.Infow("client joined/added to group while live", "jid", jid, "reason", evt.Reason)

	// If allowlist mode is active, update in-memory filter and persist to .env.
	// (If allowlist is empty/disabled, we automatically back up all groups anyway).
	if h.filter.IsAllowlistActive() {
		h.filter.AddAllowed(jid)
		h.log.Debugw("added group JID to in-memory allowlist", "jid", jid)

		if err := appendToAllowlistInEnv(".env", jid); err != nil {
			h.log.Errorw("failed to update .env GROUP_ALLOWLIST file", "error", err, "jid", jid)
		} else {
			h.log.Infow("successfully persisted new group JID to .env GROUP_ALLOWLIST", "jid", jid)
		}
	} else {
		h.log.Debugw("allowlist is not active, group will be backed up under default open/denylist mode", "jid", jid)
	}
}

// appendToAllowlistInEnv reads .env, parses it line-by-line, appends the new jid
// to the GROUP_ALLOWLIST key, and writes the contents back preserving comments.
func appendToAllowlistInEnv(path, jid string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(path, []byte("GROUP_ALLOWLIST="+jid+"\n"), 0o600)
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "GROUP_ALLOWLIST=") {
			found = true
			val := strings.TrimPrefix(trimmed, "GROUP_ALLOWLIST=")
			jids := splitCSV(val)
			exists := false
			for _, existing := range jids {
				if existing == jid {
					exists = true
					break
				}
			}
			if !exists {
				jids = append(jids, jid)
				lines[i] = "GROUP_ALLOWLIST=" + strings.Join(jids, ",")
			}
			break
		}
	}

	if !found {
		// If GROUP_ALLOWLIST line wasn't found in .env, append it.
		// Remove trailing empty lines so we append nicely.
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "GROUP_ALLOWLIST="+jid)
	}

	// Rejoin with newline and ensure a trailing newline
	output := strings.Join(lines, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return os.WriteFile(path, []byte(output), 0o600)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
