package main

import (
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Message kinds sent to the plugin. The plugin translates non-text kinds into labels.
const (
	KindText     = "text"
	KindImage    = "image"
	KindVideo    = "video"
	KindAudio    = "audio"
	KindDocument = "document"
	KindSticker  = "sticker"
	KindLocation = "location"
	KindContact  = "contact"
	KindPoll     = "poll"
)

// extractContent returns the displayable text and kind of a message.
// An empty kind means the message carries nothing to show (reactions,
// protocol messages, key distribution...) and should be ignored.
func extractContent(m *waE2E.Message) (text, kind string) {
	if m == nil {
		return "", ""
	}
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), KindText
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), KindText
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), KindImage
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption(), KindVideo
	case m.GetAudioMessage() != nil:
		return "", KindAudio
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		if d.GetCaption() != "" {
			return d.GetCaption(), KindDocument
		}
		return d.GetFileName(), KindDocument
	case m.GetStickerMessage() != nil:
		return "", KindSticker
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetName(), KindLocation
	case m.GetLiveLocationMessage() != nil:
		return "", KindLocation
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetDisplayName(), KindContact
	case m.GetContactsArrayMessage() != nil:
		return m.GetContactsArrayMessage().GetDisplayName(), KindContact
	case m.GetPollCreationMessage() != nil:
		return m.GetPollCreationMessage().GetName(), KindPoll
	case m.GetPollCreationMessageV3() != nil:
		return m.GetPollCreationMessageV3().GetName(), KindPoll
	}
	return "", ""
}
