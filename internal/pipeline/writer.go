package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AylanBoscarino/wa-backup/internal/index"
	"github.com/AylanBoscarino/wa-backup/internal/storage"
)

// Message is the on-disk representation of a single WhatsApp message.
// All optional fields use omitempty so the JSONL stays compact.
type Message struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	GroupJID   string         `json:"group_jid"`
	GroupName  string         `json:"group_name"`
	SenderJID  string         `json:"sender_jid"`
	SenderName string         `json:"sender_name,omitempty"`
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Media      *MediaRef      `json:"media,omitempty"`
	Poll       *PollData      `json:"poll,omitempty"`
	Reaction   *ReactionData  `json:"reaction,omitempty"`
	Location   *LocationData  `json:"location,omitempty"`
	ReplyTo    string         `json:"reply_to,omitempty"`
	Raw        map[string]any `json:"raw,omitempty"`
}

type MediaRef struct {
	Hash string `json:"hash"`
	Ext  string `json:"ext"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
	Path string `json:"path"`
	Name string `json:"name,omitempty"` // original filename for documents
}

type PollData struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

type ReactionData struct {
	Emoji        string `json:"emoji"`
	TargetMsgID  string `json:"target_msg_id"`
}

type LocationData struct {
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Name string  `json:"name,omitempty"`
}

// Writer serializes Message structs, hands the bytes to storage, and
// records the ingestion in the per-group index so downstream agents can
// pick up incrementally.
type Writer struct {
	store storage.Storage
	idx   *index.Manager // nil-safe; tests can leave it unset
}

// NewWriter constructs a Writer. Pass a nil index manager when running
// without incremental tracking (tests, one-off tools).
func NewWriter(s storage.Storage, idx *index.Manager) *Writer {
	return &Writer{store: s, idx: idx}
}

func (w *Writer) Append(groupKey string, msg *Message) error {
	yearMonth := msg.Timestamp.UTC().Format("2006-01")
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	if err := w.store.AppendMessage(groupKey, yearMonth, line); err != nil {
		return err
	}
	if w.idx != nil {
		w.idx.Record(msg.GroupJID, msg.GroupName, SanitizeGroupName(msg.GroupName), yearMonth, msg.ID, msg.Timestamp)
	}
	return nil
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeGroupName converts a group name into a path-safe slug:
// lowercase, spaces → hyphens, drop everything outside [a-z0-9-], trim to 64.
func SanitizeGroupName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	s = unsafeChars.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "unnamed-group"
	}
	return s
}

// HashMediaKey returns the first 16 hex chars of SHA-256(mediaKey).
// Per SPEC this dedups forwards of the same media (same MediaKey across
// chats) without requiring the file to be downloaded first. Two separate
// uploads of identical content will still produce different keys.
func HashMediaKey(mediaKey []byte) string {
	sum := sha256.Sum256(mediaKey)
	return hex.EncodeToString(sum[:])[:16]
}
