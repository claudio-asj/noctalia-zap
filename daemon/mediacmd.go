package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const downloadTimeout = 50 * time.Second

// downloadable returns the submessage of a stored raw proto that can be downloaded.
func downloadable(m *waE2E.Message) whatsmeow.DownloadableMessage {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage()
	}
	return nil
}

// ensureMedia returns the path of the media file of a message, downloading it
// when the saved file is missing.
func (d *Daemon) ensureMedia(ctx context.Context, chat, id string) (string, error) {
	ref, found, err := d.store.MediaRef(ctx, chat, id)
	if err != nil {
		log.Printf("media: store: %v", err)
		return "", errors.New("internal error")
	}
	if !found {
		return "", errors.New("not_found")
	}
	if !isMediaKind(ref.Kind) {
		return "", errors.New("not_media")
	}
	if len(ref.Raw) == 0 {
		// Stored before media support: the download keys were never kept.
		return "", errors.New("not_stored")
	}
	if ref.File != "" && fileExists(ref.File) {
		return ref.File, nil
	}
	unlock := d.downloads.Lock(mediaHash(chat, id))
	defer unlock()
	// Another request may have finished the download while this one waited.
	if ref2, _, err := d.store.MediaRef(ctx, chat, id); err == nil && ref2.File != "" && fileExists(ref2.File) {
		return ref2.File, nil
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(ref.Raw, &msg); err != nil {
		return "", errors.New("media_unavailable")
	}
	dl := downloadable(&msg)
	if dl == nil {
		return "", errors.New("media_unavailable")
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return "", err
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, err := c.Download(dctx, dl)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
			return "", errors.New("media_unavailable")
		}
		log.Printf("media download failed: %v", err)
		return "", errors.New("download failed")
	}
	var path string
	if ref.Kind == KindAudio {
		path, err = d.saveCache(chat, id, ref.Meta.Mime, data)
	} else {
		path, err = d.saveDownload(ref, msg.GetDocumentMessage().GetFileName(), data)
	}
	if err != nil {
		log.Printf("media save failed: %v", err)
		return "", errors.New("save failed")
	}
	if err := d.store.SetMediaFile(ctx, chat, id, path); err != nil {
		log.Printf("media: record file: %v", err)
	}
	if ref.Kind == KindAudio {
		if err := pruneCache(mediaDir(d.cfg.DataDir), int64(d.cfg.MediaCacheMB)<<20, path); err != nil {
			log.Printf("media: prune: %v", err)
		}
	}
	return path, nil
}

// ensurePlayable returns a file the player can play: the media itself, or the
// sound of a sticker with sound (cached like audio messages).
func (d *Daemon) ensurePlayable(ctx context.Context, chat, id string) (string, error) {
	ref, found, err := d.store.MediaRef(ctx, chat, id)
	if err != nil {
		return "", errors.New("internal error")
	}
	if !found || ref.Kind != KindSticker {
		return d.ensureMedia(ctx, chat, id)
	}
	if len(ref.Raw) == 0 {
		return "", errors.New("not_stored")
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(ref.Raw, &msg); err != nil {
		return "", errors.New("media_unavailable")
	}
	sound := msg.GetStickerMessage().GetAudioMessage()
	if sound == nil {
		return "", errors.New("not_media")
	}
	soundID := id + "#sound"
	path := cacheFilePath(d.cfg.DataDir, chat, soundID, sound.GetMimetype())
	if fileExists(path) {
		return path, nil
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return "", err
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, err := c.Download(dctx, sound)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
			return "", errors.New("media_unavailable")
		}
		log.Printf("sticker sound download failed: %v", err)
		return "", errors.New("download failed")
	}
	path, err = d.saveCache(chat, soundID, sound.GetMimetype(), data)
	if err != nil {
		return "", errors.New("save failed")
	}
	if err := pruneCache(mediaDir(d.cfg.DataDir), int64(d.cfg.MediaCacheMB)<<20, path); err != nil {
		log.Printf("media: prune: %v", err)
	}
	return path, nil
}

func (d *Daemon) saveCache(chat, id, mimeType string, data []byte) (string, error) {
	if err := os.MkdirAll(mediaDir(d.cfg.DataDir), 0o700); err != nil {
		return "", err
	}
	path := cacheFilePath(d.cfg.DataDir, chat, id, mimeType)
	return path, writeFileAtomic(path, data)
}

func (d *Daemon) saveDownload(ref MediaRef, docName string, data []byte) (string, error) {
	if err := ensureDownloadDir(d.cfg.DownloadDir); err != nil {
		return "", err
	}
	return saveUnique(d.cfg.DownloadDir, downloadName(ref.Kind, ref.TS, ref.Meta.Mime, docName), data)
}

func (d *Daemon) cmdMedia(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if req.ID == "" {
		return Response{Error: "missing id"}
	}
	path, err := d.ensureMedia(ctx, jid.String(), req.ID)
	if err != nil {
		return fail(err)
	}
	return Response{OK: true, Data: map[string]any{"path": path}}
}

func (d *Daemon) cmdPlay(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if req.ID == "" {
		return Response{Error: "missing id"}
	}
	if playerArgv("") == nil {
		return Response{Error: "no_player"}
	}
	path, err := d.ensurePlayable(ctx, jid.String(), req.ID)
	if err != nil {
		return fail(err)
	}
	seconds := 0
	if ref, found, err := d.store.MediaRef(ctx, jid.String(), req.ID); err == nil && found {
		seconds = ref.Meta.Seconds
	}
	if seconds <= 0 {
		seconds = probeSeconds(ctx, path)
	}
	if err := d.player.Play(jid.String(), req.ID, path, seconds); err != nil {
		if errors.Is(err, errNoPlayer) {
			return Response{Error: "no_player"}
		}
		log.Printf("play: %v", err)
		return Response{Error: "play failed"}
	}
	return Response{OK: true}
}
