package handler

import (
	"context"

	"github.com/AylanBoscarino/wa-backup/internal/pipeline"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

// MessageHandler forwards live events.Message to the pipeline processor.
type MessageHandler struct {
	proc *pipeline.Processor
	log  *zap.SugaredLogger
}

func NewMessageHandler(p *pipeline.Processor, log *zap.SugaredLogger) *MessageHandler {
	return &MessageHandler{proc: p, log: log}
}

func (h *MessageHandler) Handle(ctx context.Context, evt *events.Message) {
	if evt == nil {
		return
	}
	h.proc.Process(ctx, evt.Info, evt.Message)
}
