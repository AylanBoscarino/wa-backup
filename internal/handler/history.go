package handler

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AylanBoscarino/wa-backup/internal/pipeline"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

// HistoryHandler processes events.HistorySync batches through the same
// pipeline used for live messages. End-of-sync is detected by an idle
// timeout (no new HistorySync events for `idle` duration).
type HistoryHandler struct {
	proc *pipeline.Processor
	log  *zap.SugaredLogger
	idle time.Duration

	mu       sync.Mutex
	lastEvt  time.Time
	totalMsg int64
}

func NewHistoryHandler(p *pipeline.Processor, idle time.Duration, log *zap.SugaredLogger) *HistoryHandler {
	return &HistoryHandler{proc: p, log: log, idle: idle}
}

// Handle ingests one events.HistorySync. Each conversation/message is
// fed through the processor exactly like a live events.Message.
func (h *HistoryHandler) Handle(ctx context.Context, evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	h.mu.Lock()
	h.lastEvt = time.Now()
	h.mu.Unlock()

	syncType := evt.Data.GetSyncType().String()
	conversations := evt.Data.GetConversations()
	var batchMsgs int

	for _, conv := range conversations {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			h.log.Debugw("skipping conversation with unparsable JID", "id", conv.GetID(), "error", err)
			continue
		}
		if chatJID.Server != types.GroupServer {
			continue // chats individuais fora de escopo nesta fase
		}
		if name := conv.GetName(); name != "" {
			h.proc.SeedGroupName(chatJID, name)
		}

		for _, hsMsg := range conv.GetMessages() {
			wmi := hsMsg.GetMessage()
			if wmi == nil {
				continue
			}
			info := buildMessageInfo(chatJID, wmi)
			if info.Sender.IsEmpty() {
				continue
			}
			h.proc.Process(ctx, info, wmi.GetMessage())
			batchMsgs++
		}
	}

	atomic.AddInt64(&h.totalMsg, int64(batchMsgs))
	h.log.Infow("history sync batch processed",
		"sync_type", syncType,
		"conversations", len(conversations),
		"messages", batchMsgs,
		"total_messages", atomic.LoadInt64(&h.totalMsg),
	)
}

// WaitForCompletion blocks until no new HistorySync events have arrived
// for `idle` duration (per SPEC) or ctx is canceled. It is safe to call
// concurrently with Handle.
func (h *HistoryHandler) WaitForCompletion(ctx context.Context) {
	ticker := time.NewTicker(h.idle / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.mu.Lock()
			last := h.lastEvt
			h.mu.Unlock()
			if last.IsZero() {
				continue // sync hasn't started yet
			}
			if time.Since(last) >= h.idle {
				h.log.Infow("history sync idle, considered complete",
					"total_messages", atomic.LoadInt64(&h.totalMsg),
				)
				return
			}
		}
	}
}

// Total returns the number of messages ingested via history sync so far.
func (h *HistoryHandler) Total() int64 { return atomic.LoadInt64(&h.totalMsg) }
