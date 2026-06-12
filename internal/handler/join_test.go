package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AylanBoscarino/wa-backup/internal/pipeline"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

func TestAppendToAllowlistInEnv(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "wa-backup-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	envPath := filepath.Join(tmpDir, ".env")

	// Test 1: File does not exist
	err = appendToAllowlistInEnv(envPath, "group1@g.us")
	if err != nil {
		t.Fatalf("expected no error on non-existent file, got: %v", err)
	}

	content, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read .env: %v", err)
	}
	if strings.TrimSpace(string(content)) != "GROUP_ALLOWLIST=group1@g.us" {
		t.Errorf("unexpected content: %q", string(content))
	}

	// Test 2: Appending to existing file
	err = appendToAllowlistInEnv(envPath, "group2@g.us")
	if err != nil {
		t.Fatalf("expected no error on append, got: %v", err)
	}

	content, err = os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read .env: %v", err)
	}
	if !strings.Contains(string(content), "GROUP_ALLOWLIST=group1@g.us,group2@g.us") {
		t.Errorf("unexpected content: %q", string(content))
	}

	// Test 3: Idempotency (adding group1 again)
	err = appendToAllowlistInEnv(envPath, "group1@g.us")
	if err != nil {
		t.Fatalf("expected no error on duplicate append, got: %v", err)
	}

	content, err = os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read .env: %v", err)
	}
	if !strings.Contains(string(content), "GROUP_ALLOWLIST=group1@g.us,group2@g.us") {
		t.Errorf("unexpected content (should be unchanged): %q", string(content))
	}
}

func TestJoinHandler(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "wa-backup-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Switch working directory to temp directory for .env writing
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to change wd: %v", err)
	}
	defer os.Chdir(oldWd)

	// Create a dynamic filter with an active allowlist
	filter := pipeline.NewGroupFilter([]string{"existing_group@g.us"}, nil)
	log := zap.NewNop().Sugar()
	h := NewJoinHandler(filter, log)

	groupJID, _ := types.ParseJID("new_group@g.us")
	evt := &events.JoinedGroup{
		GroupInfo: types.GroupInfo{
			JID: groupJID,
		},
		Reason: "invite",
	}

	h.Handle(context.Background(), evt)

	// Verify in-memory allowlist was updated
	if !filter.Allow("new_group@g.us") {
		t.Error("expected new_group@g.us to be allowed in-memory")
	}

	// Verify .env was written and contains the JID
	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("failed to read .env: %v", err)
	}
	if !strings.Contains(string(content), "new_group@g.us") {
		t.Errorf("expected JID in .env, got: %q", string(content))
	}
}
