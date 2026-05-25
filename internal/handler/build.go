package handler

import (
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

// buildMessageInfo translates a WebMessageInfo (as found in HistorySync)
// into the same types.MessageInfo shape we get from a live events.Message,
// so the pipeline can stay protocol-agnostic.
func buildMessageInfo(chatJID types.JID, wmi *waWeb.WebMessageInfo) types.MessageInfo {
	key := wmi.GetKey()

	var sender types.JID
	if p := wmi.GetParticipant(); p != "" {
		if parsed, err := types.ParseJID(p); err == nil {
			sender = parsed
		}
	}
	if sender.IsEmpty() {
		// In groups the sender lives in participant; for 1:1 chats the
		// chat JID itself is the other party. If FromMe is true we still
		// need a non-empty sender — leave it to chatJID so the caller can
		// at least record provenance.
		if key.GetFromMe() {
			// Best effort: use the chat JID; for groups this is wrong but
			// from-me messages in groups always carry Participant.
			sender = chatJID
		} else {
			sender = chatJID
		}
	}

	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chatJID,
			Sender:   sender,
			IsFromMe: key.GetFromMe(),
			IsGroup:  chatJID.Server == types.GroupServer,
		},
		ID:        key.GetID(),
		Type:      "text",
		PushName:  wmi.GetPushName(),
		Timestamp: time.Unix(int64(wmi.GetMessageTimestamp()), 0),
	}
}
