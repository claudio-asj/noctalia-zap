package main

import (
	"bytes"
	"context"
	"image/png"
	"log"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"golang.org/x/image/webp"
	"google.golang.org/protobuf/proto"
)

// maxStickerThumb is the largest sticker downloaded just to show a preview.
const maxStickerThumb = 512 * 1024

// fetchMissingThumbs downloads previews that a history resend did not embed:
// the thumbnail of photos and videos, and small stickers (converted to PNG).
// It runs in the background, one download at a time, and then calls done with
// whether any preview was written.
func (d *Daemon) fetchMissingThumbs(chat string, done func(wrote bool)) {
	wrote := false
	defer func() { done(wrote) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ids, err := d.store.MediaIDs(ctx, chat, d.cfg.HistoryLimit*2)
	if err != nil {
		log.Printf("thumbs: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		ref, found, err := d.store.MediaRef(ctx, chat, id)
		if err != nil || !found {
			continue
		}
		path := thumbFilePath(d.cfg.DataDir, chat, id, ref.Kind)
		if fileExists(path) {
			continue
		}
		data := d.downloadPreview(ctx, ref)
		if len(data) == 0 {
			continue
		}
		if err := writeThumb(d.cfg.DataDir, chat, id, ref.Kind, data); err != nil {
			log.Printf("write thumb: %v", err)
			continue
		}
		wrote = true
	}
}

// downloadPreview returns preview bytes for one message, or nil.
func (d *Daemon) downloadPreview(ctx context.Context, ref MediaRef) []byte {
	c, _, err := d.connectedClient()
	if err != nil {
		return nil
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(ref.Raw, &msg); err != nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch {
	case msg.GetImageMessage().GetThumbnailDirectPath() != "":
		data, err := c.DownloadThumbnail(dctx, msg.GetImageMessage())
		if err == nil {
			return data
		}
	case msg.GetVideoMessage().GetThumbnailDirectPath() != "":
		data, err := c.DownloadThumbnail(dctx, msg.GetVideoMessage())
		if err == nil {
			return data
		}
	case msg.GetStickerMessage() != nil:
		st := msg.GetStickerMessage()
		if st.GetFileLength() > maxStickerThumb {
			return nil
		}
		data, err := c.Download(dctx, st)
		if err != nil {
			return nil
		}
		return webpToPNG(data)
	}
	return nil
}

// webpToPNG converts a still WebP sticker to PNG (animated ones decode their
// first frame or fail, which just leaves the sticker without a preview).
func webpToPNG(data []byte) []byte {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
