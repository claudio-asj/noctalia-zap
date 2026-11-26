package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.mau.fi/whatsmeow"
	"google.golang.org/protobuf/proto"
)

func TestDetectAndChooseKind(t *testing.T) {
	var pngHead bytes.Buffer
	_ = png.Encode(&pngHead, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	var jpgHead bytes.Buffer
	_ = jpeg.Encode(&jpgHead, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	mp4 := append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00"), make([]byte, 16)...)
	ogg := append([]byte("OggS\x00\x02"), make([]byte, 32)...)

	cases := []struct {
		name  string
		path  string
		head  []byte
		size  int64
		ptt   bool
		mime  string
		kind  string
		voice bool
	}{
		{"small png", "/a/x.png", pngHead.Bytes(), 1 << 20, false, "image/png", KindImage, false},
		{"small jpeg wrong ext", "/a/x.png", jpgHead.Bytes(), 1 << 20, false, "image/jpeg", KindImage, false},
		{"large jpeg", "/a/x.jpg", jpgHead.Bytes(), 17 << 20, false, "image/jpeg", KindDocument, false},
		{"webp", "/a/x.webp", webp, 1000, false, "image/webp", KindDocument, false},
		{"mp4", "/a/x.mp4", mp4, 10 << 20, false, "video/mp4", KindVideo, false},
		{"large mp4", "/a/x.mp4", mp4, 65 << 20, false, "video/mp4", KindDocument, false},
		{"ogg ptt", "/a/x.ogg", ogg, 1000, true, "audio/ogg", KindAudio, true},
		{"ogg no ptt", "/a/x.opus", ogg, 1000, false, "audio/ogg", KindAudio, false},
		{"mp3 ptt ignored", "/a/x.mp3", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), 1000, true, "audio/mpeg", KindAudio, false},
		{"pdf", "/a/x.pdf", []byte("%PDF-1.4 hello"), 1000, false, "application/pdf", KindDocument, false},
		{"unknown", "/a/x.bin", []byte{0, 1, 2, 3, 4}, 10, false, "application/octet-stream", KindDocument, false},
		{"text by ext", "/a/notes.txt", []byte("hello"), 5, false, "text/plain", KindDocument, false},
	}
	for _, c := range cases {
		mt := detectMime(c.path, c.head)
		if mt != c.mime {
			t.Errorf("%s: mime %q, want %q", c.name, mt, c.mime)
		}
		kind, voice := chooseKind(mt, c.size, c.ptt)
		if kind != c.kind || voice != c.voice {
			t.Errorf("%s: kind %q voice %v, want %q %v", c.name, kind, voice, c.kind, c.voice)
		}
	}
}

func TestMakeThumb(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := range 400 {
		for y := range 200 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 80, 255})
		}
	}
	var src bytes.Buffer
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	thumb, w, h, err := makeThumb(src.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if w != 400 || h != 200 {
		t.Errorf("original size %dx%d", w, h)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("thumb is not a JPEG: %v", err)
	}
	if cfg.Width != 100 || cfg.Height != 50 {
		t.Errorf("thumb %dx%d, want 100x50", cfg.Width, cfg.Height)
	}
	if _, _, _, err := makeThumb([]byte("not an image")); err == nil {
		t.Error("expected error for non-image")
	}
}

func TestBuildMediaMessage(t *testing.T) {
	up := whatsmeow.UploadResponse{URL: "u", DirectPath: "p", MediaKey: []byte{1}, FileLength: 9}
	m := buildMediaMessage(sendPlan{kind: KindAudio, mime: "audio/ogg", ptt: true, seconds: 7}, up, "ignored", nil)
	a := m.GetAudioMessage()
	if a == nil || !a.GetPTT() || a.GetSeconds() != 7 || a.GetMimetype() != "audio/ogg; codecs=opus" {
		t.Fatalf("audio = %v", a)
	}
	m = buildMediaMessage(sendPlan{kind: KindDocument, mime: "application/pdf", name: "a.pdf"}, up, "cap", nil)
	if d := m.GetDocumentMessage(); d.GetFileName() != "a.pdf" || d.GetCaption() != "cap" {
		t.Fatalf("document = %v", d)
	}
	m = buildMediaMessage(sendPlan{kind: KindImage, mime: "image/png", w: 3, h: 4, thumb: []byte{1}}, up, "", nil)
	if i := m.GetImageMessage(); i.GetWidth() != 3 || i.Caption != nil || !proto.Equal(m, m) {
		t.Fatalf("image = %v", i)
	}
}

func TestParseCtlNewCommands(t *testing.T) {
	for _, c := range []string{"pick", "paste"} {
		req, err := parseCtlArgs([]string{c})
		if err != nil || req.Cmd != c {
			t.Errorf("%s: %+v %v", c, req, err)
		}
		if _, err := parseCtlArgs([]string{c, "x"}); err == nil {
			t.Errorf("%s with args should fail", c)
		}
	}
	for _, s := range []string{"start", "stop", "cancel"} {
		req, err := parseCtlArgs([]string{"record", s})
		if err != nil || req.Cmd != "record" || req.Text != s {
			t.Errorf("record %s: %+v %v", s, req, err)
		}
	}
	for _, bad := range [][]string{{"record"}, {"record", "pause"}, {"record", "start", "x"}} {
		if _, err := parseCtlArgs(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
	req, err := parseCtlArgs([]string{"send", "a@s.whatsapp.net", "--media", "/tmp/a.ogg", "--ptt", "--file", "/tmp/cap", "--reply", "ID1"})
	want := Request{Cmd: "send", Chat: "a@s.whatsapp.net", Media: "/tmp/a.ogg", PTT: true, File: "/tmp/cap", Reply: "ID1"}
	if err != nil || !reflect.DeepEqual(req, want) {
		t.Fatalf("send media: %+v %v", req, err)
	}
	req, err = parseCtlArgs([]string{"send", "a@s.whatsapp.net", "--media", "/tmp/a.png"})
	if err != nil || req.Media != "/tmp/a.png" || req.PTT || req.File != "" || req.Reply != "" {
		t.Fatalf("send media simple: %+v %v", req, err)
	}
	for _, bad := range [][]string{
		{"send", "a@s.whatsapp.net", "--media"},
		{"send", "--media", "/x"},
		{"send", "a@s.whatsapp.net", "--media", "/x", "--bogus", "y"},
		{"send", "a@s.whatsapp.net", "--media", "/x", "--reply"},
	} {
		if _, err := parseCtlArgs(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
	// The old text forms still work.
	if req, err := parseCtlArgs([]string{"send", "a@s.whatsapp.net", "hi", "--reply", "ID"}); err != nil || req.Text != "hi" || req.Reply != "ID" {
		t.Errorf("text send: %+v %v", req, err)
	}
}

func TestCleanOutbox(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	if err := cleanOutbox(dir, time.Hour, time.Now()); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if err := ensureOutbox(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, age := range map[string]time.Duration{"old.png": 30 * time.Hour, "new.png": time.Hour} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	if err := cleanOutbox(dir, outboxMaxAge, now); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(dir, "old.png")) || !fileExists(filepath.Join(dir, "new.png")) {
		t.Error("cleanup removed the wrong files")
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("outbox mode %v", fi.Mode().Perm())
	}
	if !inOutbox(dir, filepath.Join(dir, "a.png")) || inOutbox(dir, "/elsewhere/a.png") {
		t.Error("inOutbox wrong")
	}
}

func TestPlaybackEventFields(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	em.Emit(PlaybackEvent{Type: "playback", Chat: "c", ID: "i", State: "playing", StartedMS: 1700000000123, Seconds: 12})
	em.Emit(PlaybackEvent{Type: "playback", Chat: "c", ID: "i", State: "stopped"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != `{"type":"playback","chat":"c","id":"i","state":"playing","started_ms":1700000000123,"seconds":12}` {
		t.Errorf("playing line %s", lines[0])
	}
	if lines[1] != `{"type":"playback","chat":"c","id":"i","state":"stopped"}` {
		t.Errorf("stopped line %s", lines[1])
	}
}

func TestPlayEmitsProgressFields(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nexec /bin/sleep 5\n"
	if err := os.WriteFile(filepath.Join(bin, "mpv"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var buf bytes.Buffer
	p := NewPlayer(NewEmitter(&buf))
	before := time.Now().UnixMilli()
	if err := p.Play("c", "i", "/x.ogg", 9); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	out := buf.String()
	if !strings.Contains(out, `"seconds":9`) || !strings.Contains(out, `"started_ms":`) {
		t.Fatalf("events: %s", out)
	}
	if time.Now().UnixMilli() < before {
		t.Fatal("clock")
	}
}

func TestParseProbeAndSeconds(t *testing.T) {
	p := parseProbe([]byte(`{"streams":[{"codec_type":"audio"},{"codec_type":"video","width":640,"height":360}],"format":{"duration":"7.4"}}`))
	if p.W != 640 || p.H != 360 || p.Seconds != 7.4 {
		t.Errorf("probe %+v", p)
	}
	if wholeSeconds(0.2) != 1 || wholeSeconds(7.4) != 7 || wholeSeconds(0) != 0 {
		t.Error("wholeSeconds")
	}
	_ = context.Background()
}

func TestPickedPathAndPasteType(t *testing.T) {
	res := func(u ...string) map[string]dbus.Variant { return map[string]dbus.Variant{"uris": dbus.MakeVariant(u)} }
	if p, ok := pickedPath(0, res("file:///home/a/My%20File.png")); !ok || p != "/home/a/My File.png" {
		t.Errorf("path %q %v", p, ok)
	}
	if _, ok := pickedPath(1, res("file:///a")); ok {
		t.Error("cancel code accepted")
	}
	if _, ok := pickedPath(0, res("http://x/a")); ok {
		t.Error("non-file URI accepted")
	}
	if _, ok := pickedPath(0, map[string]dbus.Variant{}); ok {
		t.Error("missing uris accepted")
	}
	if got := pickPasteType("text/plain\nimage/jpeg\nimage/png\n"); got != "image/png" {
		t.Errorf("paste type %q", got)
	}
	if got := pickPasteType("text/plain\nimage/gif\n"); got != "image/gif" || pickPasteType("text/plain") != "" {
		t.Error("paste fallback")
	}
}
