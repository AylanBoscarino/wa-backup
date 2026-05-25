package pipeline

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
)

// GroupFilter decides whether a given group JID should be processed.
type GroupFilter struct {
	allow map[string]struct{}
	deny  map[string]struct{}
}

func NewGroupFilter(allowlist, denylist []string) GroupFilter {
	f := GroupFilter{}
	if len(allowlist) > 0 {
		f.allow = make(map[string]struct{}, len(allowlist))
		for _, j := range allowlist {
			f.allow[j] = struct{}{}
		}
	}
	if len(denylist) > 0 {
		f.deny = make(map[string]struct{}, len(denylist))
		for _, j := range denylist {
			f.deny[j] = struct{}{}
		}
	}
	return f
}

func (g GroupFilter) Allow(jid string) bool {
	if g.allow != nil {
		_, ok := g.allow[jid]
		return ok
	}
	if g.deny != nil {
		_, ok := g.deny[jid]
		return !ok
	}
	return true
}

// Processor turns a (MessageInfo, *waE2E.Message) pair into JSONL output.
// It is safe to call Process concurrently.
type Processor struct {
	cli        *whatsmeow.Client
	writer     *Writer
	downloader *Downloader
	filter     GroupFilter
	log        *zap.SugaredLogger

	mu        sync.RWMutex
	groupName map[types.JID]string

	processed int64
}

func NewProcessor(cli *whatsmeow.Client, w *Writer, d *Downloader, f GroupFilter, log *zap.SugaredLogger) *Processor {
	return &Processor{
		cli:        cli,
		writer:     w,
		downloader: d,
		filter:     f,
		log:        log,
		groupName:  make(map[types.JID]string),
	}
}

// SeedGroupName lets the history-sync handler pre-populate the cache so we
// don't have to ask the server for every group when it already gave us the
// name in the conversation header.
func (p *Processor) SeedGroupName(jid types.JID, name string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	p.groupName[jid] = name
	p.mu.Unlock()
}

func (p *Processor) Stats() int64 { return atomic.LoadInt64(&p.processed) }

func (p *Processor) Process(ctx context.Context, info types.MessageInfo, msg *waE2E.Message) {
	if !info.IsGroup {
		return // chats individuais não fazem parte desta fase
	}
	if msg == nil {
		return
	}
	chatJID := info.Chat.String()
	if !p.filter.Allow(chatJID) {
		return
	}

	groupName := p.resolveGroupName(ctx, info.Chat)
	slug := SanitizeGroupName(groupName)
	yearMonth := info.Timestamp.UTC().Format("2006-01")

	env := &Message{
		ID:         info.ID,
		Timestamp:  info.Timestamp.UTC(),
		GroupJID:   chatJID,
		GroupName:  groupName,
		SenderJID:  info.Sender.String(),
		SenderName: info.PushName,
		ReplyTo:    extractReplyTo(msg),
	}

	atomic.AddInt64(&p.processed, 1)
	p.log.Debugw("processing message", "id", info.ID, "group", groupName, "type_hint", info.Type)

	switch {
	case msg.GetImageMessage() != nil:
		p.enqueueMedia(ctx, env, slug, yearMonth, "image", msg.GetImageMessage())
	case msg.GetVideoMessage() != nil:
		p.enqueueMedia(ctx, env, slug, yearMonth, "video", msg.GetVideoMessage())
	case msg.GetAudioMessage() != nil:
		p.enqueueMedia(ctx, env, slug, yearMonth, "audio", msg.GetAudioMessage())
	case msg.GetDocumentMessage() != nil:
		p.enqueueMedia(ctx, env, slug, yearMonth, "document", msg.GetDocumentMessage())
	case msg.GetStickerMessage() != nil:
		p.enqueueMedia(ctx, env, slug, yearMonth, "sticker", msg.GetStickerMessage())
	case msg.GetPollCreationMessage() != nil || msg.GetPollCreationMessageV2() != nil || msg.GetPollCreationMessageV3() != nil:
		p.handlePoll(env, slug, msg)
	case msg.GetReactionMessage() != nil:
		p.handleReaction(env, slug, msg.GetReactionMessage())
	case msg.GetLocationMessage() != nil:
		p.handleLocation(env, slug, msg.GetLocationMessage())
	case msg.GetExtendedTextMessage() != nil:
		env.Type = "text"
		env.Text = msg.GetExtendedTextMessage().GetText()
		p.appendOrLog(slug, env)
	case msg.GetConversation() != "":
		env.Type = "text"
		env.Text = msg.GetConversation()
		p.appendOrLog(slug, env)
	default:
		p.handleUnknown(env, slug, msg)
	}
}

func (p *Processor) handlePoll(env *Message, slug string, msg *waE2E.Message) {
	poll := msg.GetPollCreationMessage()
	if poll == nil {
		poll = msg.GetPollCreationMessageV2()
	}
	if poll == nil {
		poll = msg.GetPollCreationMessageV3()
	}
	if poll == nil {
		p.handleUnknown(env, slug, msg)
		return
	}
	opts := make([]string, 0, len(poll.GetOptions()))
	for _, o := range poll.GetOptions() {
		opts = append(opts, o.GetOptionName())
	}
	env.Type = "poll"
	env.Poll = &PollData{Question: poll.GetName(), Options: opts}
	p.appendOrLog(slug, env)
}

func (p *Processor) handleReaction(env *Message, slug string, r *waE2E.ReactionMessage) {
	env.Type = "reaction"
	env.Reaction = &ReactionData{
		Emoji:       r.GetText(),
		TargetMsgID: r.GetKey().GetID(),
	}
	p.appendOrLog(slug, env)
}

func (p *Processor) handleLocation(env *Message, slug string, loc *waE2E.LocationMessage) {
	env.Type = "location"
	env.Location = &LocationData{
		Lat:  loc.GetDegreesLatitude(),
		Lon:  loc.GetDegreesLongitude(),
		Name: loc.GetName(),
	}
	p.appendOrLog(slug, env)
}

func (p *Processor) handleUnknown(env *Message, slug string, msg *waE2E.Message) {
	env.Type = "unknown"
	// Best-effort raw dump so nothing is lost. protojson keeps the field
	// names as defined in the .proto, which is the most stable encoding.
	if raw, err := protojson.Marshal(msg); err == nil {
		var asMap map[string]any
		if err := json.Unmarshal(raw, &asMap); err == nil {
			env.Raw = asMap
		}
	}
	p.appendOrLog(slug, env)
}

func (p *Processor) appendOrLog(slug string, env *Message) {
	if err := p.writer.Append(slug, env); err != nil {
		p.log.Errorw("append message failed", "error", err, "id", env.ID)
	}
}

// downloadable is the intersection of methods we need from every media
// message type to enqueue a job.
type downloadable interface {
	whatsmeow.DownloadableMessage
	GetMimetype() string
	GetMediaKey() []byte
	GetFileLength() uint64
}

func (p *Processor) enqueueMedia(ctx context.Context, env *Message, slug, yearMonth, kind string, payload downloadable) {
	env.Type = kind
	// Captions / filenames vary by media type; pull what's available.
	switch m := payload.(type) {
	case *waE2E.ImageMessage:
		env.Text = m.GetCaption()
	case *waE2E.VideoMessage:
		env.Text = m.GetCaption()
	case *waE2E.DocumentMessage:
		env.Text = m.GetCaption()
	}

	mediaKey := payload.GetMediaKey()
	if len(mediaKey) == 0 {
		// Without a MediaKey we can't dedup nor decrypt; persist the
		// envelope and move on.
		p.log.Warnw("media without MediaKey, skipping download", "id", env.ID, "type", kind)
		p.appendOrLog(slug, env)
		return
	}
	hash := HashMediaKey(mediaKey)
	mimeType := payload.GetMimetype()
	ext := extFromMime(mimeType)
	var origName string
	if doc, ok := payload.(*waE2E.DocumentMessage); ok {
		origName = doc.GetFileName()
		if e := ExtFromFilename(origName); e != "" {
			ext = e
		}
	}

	p.downloader.Enqueue(ctx, &MediaJob{
		GroupSlug:        slug,
		YearMonth:        yearMonth,
		Hash:             hash,
		Ext:              ext,
		Mime:             mimeType,
		Source:           payload,
		Envelope:         env,
		OriginalFileName: origName,
	})
}

func (p *Processor) resolveGroupName(ctx context.Context, jid types.JID) string {
	p.mu.RLock()
	if name, ok := p.groupName[jid]; ok {
		p.mu.RUnlock()
		return name
	}
	p.mu.RUnlock()

	// Ask the server. If this fails (history sync replay, offline), fall
	// back to the JID's user component so the slug stays stable.
	info, err := p.cli.GetGroupInfo(ctx, jid)
	if err != nil || info == nil {
		name := jid.User
		p.mu.Lock()
		p.groupName[jid] = name
		p.mu.Unlock()
		return name
	}
	p.mu.Lock()
	p.groupName[jid] = info.Name
	p.mu.Unlock()
	return info.Name
}

func extractReplyTo(msg *waE2E.Message) string {
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if img := msg.GetImageMessage(); img != nil {
		if ci := img.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		if ci := vid.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		if ci := aud.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		if ci := doc.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	return ""
}
