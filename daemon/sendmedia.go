package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registers the GIF decoder
	"image/jpeg"
	_ "image/png" // registers the PNG decoder
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder for image.Decode
	"google.golang.org/protobuf/proto"
)

const (
	maxSendSize  = 100 << 20
	maxImageSend = 16 << 20
	maxVideoSend = 64 << 20
	thumbMaxSide = 100
	// A ctl call gives up after about a minute; after that failures are events.
	ctlPatience   = 55 * time.Second
	uploadTimeout = 10 * time.Minute
)

// SendFailedEvent reports a media send that failed after the ctl call gave up.
type SendFailedEvent struct {
	Type  string `json:"type"` // "send_failed"
	Chat  string `json:"chat"`
	Error string `json:"error"`
}

var extMime = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".ogg": "audio/ogg", ".opus": "audio/ogg", ".oga": "audio/ogg", ".mp3": "audio/mpeg", ".m4a": "audio/mp4",
	".aac": "audio/aac", ".wav": "audio/wav", ".amr": "audio/amr", ".flac": "audio/flac",
	".pdf": "application/pdf", ".zip": "application/zip", ".txt": "text/plain",
}

// detectMime guesses the MIME type from the first bytes of a file and its
// extension. Content wins; the extension settles generic or ambiguous cases.
func detectMime(path string, head []byte) string {
	ext := extMime[strings.ToLower(filepath.Ext(path))]
	sniffed := http.DetectContentType(head)
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = sniffed[:i]
	}
	switch {
	case sniffed == "application/ogg":
		if ext == "audio/ogg" || ext == "" {
			return "audio/ogg"
		}
		return "application/ogg"
	case sniffed == "audio/wave":
		return "audio/wav"
	case sniffed == "video/mp4" && strings.HasPrefix(ext, "audio/"):
		return ext // m4a shares the mp4 container
	case sniffed == "application/octet-stream", strings.HasPrefix(sniffed, "text/"):
		if ext != "" {
			return ext
		}
	}
	return sniffed
}

// chooseKind picks the message type for a MIME type and size, and whether an
// audio goes out as a voice note (only Ogg/Opus can).
func chooseKind(mimeType string, size int64, ptt bool) (kind string, voice bool) {
	switch {
	case (mimeType == "image/jpeg" || mimeType == "image/png") && size <= maxImageSend:
		return KindImage, false
	case mimeType == "video/mp4" && size <= maxVideoSend:
		return KindVideo, false
	case strings.HasPrefix(mimeType, "audio/"):
		return KindAudio, ptt && mimeType == "audio/ogg"
	}
	return KindDocument, false
}

// makeThumb decodes an image and returns a JPEG whose longest side is at most
// thumbMaxSide, plus the original width and height.
func makeThumb(data []byte) (thumb []byte, w, h int, err error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, 0, 0, errors.New("empty image")
	}
	tw, th := w, h
	if m := max(w, h); m > thumbMaxSide {
		tw, th = max(1, w*thumbMaxSide/m), max(1, h*thumbMaxSide/m)
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 70}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

// videoFrame grabs a frame of a video with ffmpeg as PNG, nil when impossible.
func videoFrame(ctx context.Context, path string) []byte {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// Videos shorter than 0.5 s give no frame at that position: retry at 0.
	for _, ss := range []string{"0.5", "0"} {
		out, err := exec.CommandContext(ctx, bin, "-y", "-loglevel", "error", "-ss", ss, "-i", path,
			"-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1").Output()
		if err == nil && len(out) > 0 {
			return out
		}
	}
	return nil
}

// sendPlan is everything decided about a file before uploading it.
type sendPlan struct {
	kind    string
	mime    string
	name    string
	ptt     bool
	seconds int
	w, h    int
	thumb   []byte
}

// planSend detects the type and gathers dimensions, duration and preview.
func planSend(ctx context.Context, path string, data []byte, ptt bool) sendPlan {
	head := data[:min(len(data), 512)]
	p := sendPlan{mime: detectMime(path, head), name: filepath.Base(path)}
	p.kind, p.ptt = chooseKind(p.mime, int64(len(data)), ptt)
	switch p.kind {
	case KindImage:
		thumb, w, h, err := makeThumb(data)
		if err != nil {
			// Not a decodable image after all: send it as a file.
			p.kind = KindDocument
			break
		}
		p.thumb, p.w, p.h = thumb, w, h
	case KindVideo:
		pr := probeMedia(ctx, path)
		p.w, p.h, p.seconds = pr.W, pr.H, wholeSeconds(pr.Seconds)
		if frame := videoFrame(ctx, path); frame != nil {
			if thumb, w, h, err := makeThumb(frame); err == nil {
				p.thumb = thumb
				if p.w == 0 {
					p.w, p.h = w, h
				}
			}
		}
	case KindAudio:
		p.seconds = wholeSeconds(probeMedia(ctx, path).Seconds)
	}
	return p
}

// buildMediaMessage assembles the proto for an uploaded file.
func buildMediaMessage(p sendPlan, up whatsmeow.UploadResponse, caption string, ci *waE2E.ContextInfo) *waE2E.Message {
	switch p.kind {
	case KindImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			Mimetype: proto.String(p.mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Caption: optString(caption),
			JPEGThumbnail: p.thumb, Width: proto.Uint32(uint32(p.w)), Height: proto.Uint32(uint32(p.h)),
			ContextInfo: ci,
		}}
	case KindVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			Mimetype: proto.String(p.mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Caption: optString(caption),
			JPEGThumbnail: p.thumb, Width: proto.Uint32(uint32(p.w)), Height: proto.Uint32(uint32(p.h)),
			Seconds: proto.Uint32(uint32(p.seconds)), ContextInfo: ci,
		}}
	case KindAudio:
		mimeType := p.mime
		if p.ptt {
			mimeType = "audio/ogg; codecs=opus"
		}
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			Mimetype: proto.String(mimeType), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Seconds: proto.Uint32(uint32(p.seconds)),
			PTT: proto.Bool(p.ptt), ContextInfo: ci,
		}}
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
		Mimetype: proto.String(p.mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
		FileLength: proto.Uint64(up.FileLength), FileName: proto.String(p.name), Caption: optString(caption),
		ContextInfo: ci,
	}}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

func uploadType(kind string) whatsmeow.MediaType {
	switch kind {
	case KindImage:
		return whatsmeow.MediaImage
	case KindVideo:
		return whatsmeow.MediaVideo
	case KindAudio:
		return whatsmeow.MediaAudio
	}
	return whatsmeow.MediaDocument
}

// readSendFile reads a regular file of at most maxSendSize bytes.
func readSendFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("not_found")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not_found")
	}
	if fi.Size() > maxSendSize {
		return nil, errors.New("too_large")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSendSize+1))
	if err != nil {
		return nil, errors.New("read failed")
	}
	if len(data) > maxSendSize {
		return nil, errors.New("too_large")
	}
	if len(data) == 0 {
		return nil, errors.New("empty file")
	}
	return data, nil
}

// cmdSendMedia uploads a file and sends it. It may outlive the ctl call, so
// a failure after ctlPatience also comes out as a send_failed event.
func (d *Daemon) cmdSendMedia(ctx context.Context, req Request) Response {
	start := time.Now()
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if !filepath.IsAbs(req.Media) {
		return Response{Error: "media path must be absolute"}
	}
	if len(req.Text) > maxTextLength {
		return Response{Error: "text too long"}
	}
	failed := func(code string) Response {
		if time.Since(start) > ctlPatience {
			d.emit.Emit(SendFailedEvent{Type: "send_failed", Chat: jid.String(), Error: code})
		}
		return Response{Error: code}
	}
	c, me, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	data, err := readSendFile(req.Media)
	if err != nil {
		return fail(err)
	}
	ci, quote, err := d.buildReply(ctx, jid, me, req.Reply)
	if err != nil {
		return fail(err)
	}
	plan := planSend(ctx, req.Media, data, req.PTT)
	if plan.kind == KindAudio {
		req.Text = "" // captions are ignored on audio
	}
	uctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	up, err := c.Upload(uctx, data, uploadType(plan.kind))
	if err != nil {
		log.Printf("media upload failed: %v", err)
		return failed("upload failed")
	}
	outgoing := buildMediaMessage(plan, up, req.Text, ci)
	sctx, cancelSend := context.WithTimeout(ctx, 45*time.Second)
	defer cancelSend()
	resp, err := c.SendMessage(sctx, jid, outgoing)
	if err != nil {
		log.Printf("media send failed: %v", err)
		return failed("send failed")
	}
	text, kind := extractContent(outgoing)
	msg := Message{
		Quote: quote, Ack: "sent", ID: resp.ID, Chat: jid.String(), From: me.String(),
		Text: text, Kind: kind, TS: resp.Timestamp.Unix(), Me: true,
	}
	mp := extractMedia(outgoing)
	if mp != nil {
		info := mp.Info
		msg.Media, msg.Raw = &info, mp.Raw
	}
	if _, err := d.store.AddMessage(ctx, msg, false); err != nil {
		log.Printf("store sent message: %v", err)
	}
	if mp != nil {
		if err := writeThumb(d.cfg.DataDir, msg.Chat, msg.ID, kind, mp.Thumb); err != nil {
			log.Printf("write thumb: %v", err)
		}
		if !inOutbox(outboxDir(d.cfg.DataDir), req.Media) {
			if err := d.store.SetMediaFile(ctx, msg.Chat, msg.ID, req.Media); err != nil {
				log.Printf("media: record file: %v", err)
			} else {
				msg.Media.File = req.Media
			}
		}
		fillMedia(d.cfg.DataDir, &msg)
	}
	if inOutbox(outboxDir(d.cfg.DataDir), req.Media) {
		if err := os.Remove(req.Media); err != nil {
			log.Printf("outbox: remove sent file: %v", err)
		}
	}
	d.ensureChatName(ctx, jid, "")
	d.emitMessage(ctx, msg, false)
	d.scheduleChats()
	return Response{OK: true, Data: map[string]any{"id": resp.ID, "ts": msg.TS}}
}
