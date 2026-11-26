package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMigrationOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	db, err := sql.Open("sqlite3", sqliteDSN(path, "_journal_mode=WAL"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `INSERT INTO messages (chat, id, kind, ts) VALUES ('c@s.whatsapp.net', 'old', 'image', 5);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenMsgStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s2, err := OpenMsgStore(path); err != nil { // second open must be a no-op
		t.Fatal(err)
	} else {
		s2.Close()
	}
	ctx := context.Background()
	ref, found, err := s.MediaRef(ctx, "c@s.whatsapp.net", "old")
	if err != nil || !found || len(ref.Raw) != 0 || ref.Kind != KindImage {
		t.Fatalf("ref = %+v found=%v err=%v", ref, found, err)
	}
	msgs, err := s.History(ctx, "c@s.whatsapp.net", 10)
	if err != nil || len(msgs) != 1 || msgs[0].Media != nil {
		t.Fatalf("history = %+v, %v", msgs, err)
	}
	ok, err := s.AddMessage(ctx, Message{ID: "new", Chat: "c@s.whatsapp.net", Kind: KindAudio, TS: 6,
		Media: &MediaInfo{Mime: "audio/ogg", Seconds: 3, PTT: true, File: "/x", Thumb: "/y"}, Raw: []byte{1}}, false)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.SetMediaFile(ctx, "c@s.whatsapp.net", "new", "/tmp/f.ogg"); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.History(ctx, "c@s.whatsapp.net", 10)
	m := msgs[1].Media
	if m == nil || m.Seconds != 3 || !m.PTT || m.File != "/tmp/f.ogg" || m.Thumb != "" {
		t.Fatalf("media = %+v", m)
	}
}

func TestMediaHashAndExt(t *testing.T) {
	h := mediaHash("a@s.whatsapp.net", "ID1")
	if len(h) != 32 || h != mediaHash("a@s.whatsapp.net", "ID1") || h == mediaHash("a@s.whatsapp.net", "ID2") {
		t.Fatalf("bad hash %q", h)
	}
	for in, want := range map[string]string{
		"audio/ogg": ".ogg", "audio/ogg; codecs=opus": ".ogg", "image/jpeg": ".jpg",
		"application/pdf": ".pdf", "": ".bin", "x/unknown": ".bin",
	} {
		if got := extForMime(in); got != want {
			t.Errorf("extForMime(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cacheFilePath("/d", "c", "i", "audio/ogg"); got != "/d/media/"+mediaHash("c", "i")+".ogg" {
		t.Errorf("cache path %q", got)
	}
}

func TestPruneCache(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, n := range []string{"a", "b", "c", "keep"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	// keep is the newest here; make it the oldest to prove it survives.
	old := base.Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "keep"), old, old)
	if err := os.Mkdir(filepath.Join(dir, "thumbs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneCache(dir, 250, filepath.Join(dir, "keep")); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]bool{"a": false, "b": false, "c": true, "keep": true, "thumbs": true} {
		_, err := os.Stat(filepath.Join(dir, n))
		if (err == nil) != want {
			t.Errorf("%s exists=%v want %v", n, err == nil, want)
		}
	}
}

func TestDefaultDownloadDir(t *testing.T) {
	home := t.TempDir()
	if got := defaultDownloadDir(home); got != filepath.Join(home, "Downloads", "WhatsApp") {
		t.Errorf("no file: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := "# comment\nXDG_DESKTOP_DIR=\"$HOME/Desktop\"\nXDG_DOWNLOAD_DIR=\"$HOME/Baixados\"\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "user-dirs.dirs"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultDownloadDir(home); got != filepath.Join(home, "Baixados", "WhatsApp") {
		t.Errorf("user-dirs: %q", got)
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":        "report.pdf",
		"../../etc/passwd":  "_._etc_passwd",
		"a/b\\c.txt":        "a_b_c.txt",
		"bad\x00\x07name.x": "bad__name.x",
		"..":                "",
		"  ":                "",
		"..hidden":          "hidden",
		"a..b.txt":          "a.b.txt",
	} {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 100) + ".pdf"
	got := sanitizeFileName(long)
	if len(got) > maxFileNameBytes || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
}

func TestDownloadName(t *testing.T) {
	ts := time.Date(2024, 3, 5, 14, 7, 9, 0, time.Local).Unix()
	if got := downloadName(KindImage, ts, "image/jpeg", ""); got != "WhatsApp Image 2024-03-05 14.07.09.jpg" {
		t.Errorf("image: %q", got)
	}
	if got := downloadName(KindSticker, ts, "image/webp", ""); got != "WhatsApp Sticker 2024-03-05 14.07.09.webp" {
		t.Errorf("sticker: %q", got)
	}
	if got := downloadName(KindDocument, ts, "application/pdf", "a/b.pdf"); got != "a_b.pdf" {
		t.Errorf("doc: %q", got)
	}
	if got := downloadName(KindDocument, ts, "application/pdf", ""); got != "WhatsApp Document 2024-03-05 14.07.09.pdf" {
		t.Errorf("doc fallback: %q", got)
	}
}

func TestSaveUniqueCollisions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "WhatsApp")
	if err := ensureDownloadDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	var paths []string
	for i := 0; i < 3; i++ {
		p, err := saveUnique(dir, "doc.pdf", []byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(p))
	}
	want := []string{"doc.pdf", "doc (1).pdf", "doc (2).pdf"}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v", paths)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "doc.pdf"))
	if len(b) != 1 || b[0] != 0 {
		t.Error("first file was overwritten")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "doc.pdf")); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
}

func TestParseCtlMedia(t *testing.T) {
	r, err := parseCtlArgs([]string{"media", "a@s.whatsapp.net", "ID"})
	if err != nil || r.Cmd != "media" || r.Chat != "a@s.whatsapp.net" || r.ID != "ID" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = parseCtlArgs([]string{"play", "c", "i"}); err != nil || r.Cmd != "play" || r.ID != "i" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = parseCtlArgs([]string{"stop"}); err != nil || r.Cmd != "stop" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range [][]string{{"media"}, {"media", "c"}, {"play", "c", "i", "x"}, {"stop", "x"}} {
		if _, err := parseCtlArgs(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestStickerWithSound(t *testing.T) {
	msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
		Mimetype: proto.String("image/webp"),
		Audio: &waE2E.StickerMessage_AudioMessage{AudioMessage: &waE2E.AudioMessage{
			Seconds: proto.Uint32(3), Mimetype: proto.String("audio/ogg"),
		}},
	}}
	p := extractMedia(msg)
	if p == nil || !p.Info.Sound || p.Info.Seconds != 3 {
		t.Fatalf("sticker with sound not detected: %+v", p)
	}
	wrapped := &waE2E.Message{AudioStickerMessage: &waE2E.FutureProofMessage{Message: msg}}
	if _, kind := extractContent(wrapped.GetAudioStickerMessage().GetMessage()); kind != KindSticker {
		t.Fatalf("unwrapped kind = %q", kind)
	}
}
