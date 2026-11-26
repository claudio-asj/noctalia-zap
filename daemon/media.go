package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// MediaInfo is the optional "media" object of a message. Every field is
// optional. The database keeps only the metadata (no paths); Thumb and File are
// filled in when a message is read or announced, if the files exist.
type MediaInfo struct {
	Thumb   string `json:"thumb,omitempty"`
	W       int    `json:"w,omitempty"`
	H       int    `json:"h,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Mime    string `json:"mime,omitempty"`
	PTT     bool   `json:"ptt,omitempty"`
	Sound   bool   `json:"sound,omitempty"` // sticker with sound
	File    string `json:"file,omitempty"`
}

const (
	mediaDirName  = "media"
	thumbsDirName = "thumbs"
	partSuffix    = ".part"
)

func isMediaKind(kind string) bool {
	switch kind {
	case KindImage, KindVideo, KindAudio, KindDocument, KindSticker:
		return true
	}
	return false
}

// mediaPayload is what handleMessage keeps for a media message.
type mediaPayload struct {
	Info  MediaInfo
	Thumb []byte // preview image, may be empty
	Raw   []byte // marshaled waE2E.Message with only the media submessage, no thumbnail
}

// extractMedia returns the media data of a message, or nil when it has none.
func extractMedia(m *waE2E.Message) *mediaPayload {
	if m == nil {
		return nil
	}
	p := &mediaPayload{}
	out := &waE2E.Message{}
	switch {
	case m.GetImageMessage() != nil:
		x := proto.Clone(m.GetImageMessage()).(*waE2E.ImageMessage)
		p.Thumb = x.GetJPEGThumbnail()
		x.JPEGThumbnail = nil
		p.Info = MediaInfo{W: int(x.GetWidth()), H: int(x.GetHeight()), Size: int64(x.GetFileLength()), Mime: x.GetMimetype()}
		out.ImageMessage = x
	case m.GetVideoMessage() != nil:
		x := proto.Clone(m.GetVideoMessage()).(*waE2E.VideoMessage)
		p.Thumb = x.GetJPEGThumbnail()
		x.JPEGThumbnail = nil
		p.Info = MediaInfo{W: int(x.GetWidth()), H: int(x.GetHeight()), Seconds: int(x.GetSeconds()),
			Size: int64(x.GetFileLength()), Mime: x.GetMimetype()}
		out.VideoMessage = x
	case m.GetAudioMessage() != nil:
		x := proto.Clone(m.GetAudioMessage()).(*waE2E.AudioMessage)
		p.Info = MediaInfo{Seconds: int(x.GetSeconds()), Size: int64(x.GetFileLength()), Mime: x.GetMimetype(), PTT: x.GetPTT()}
		out.AudioMessage = x
	case m.GetDocumentMessage() != nil:
		x := proto.Clone(m.GetDocumentMessage()).(*waE2E.DocumentMessage)
		p.Thumb = x.GetJPEGThumbnail()
		x.JPEGThumbnail = nil
		p.Info = MediaInfo{W: int(x.GetThumbnailWidth()), H: int(x.GetThumbnailHeight()),
			Size: int64(x.GetFileLength()), Mime: x.GetMimetype()}
		out.DocumentMessage = x
	case m.GetStickerMessage() != nil:
		x := proto.Clone(m.GetStickerMessage()).(*waE2E.StickerMessage)
		p.Thumb = x.GetPngThumbnail()
		x.PngThumbnail = nil
		p.Info = MediaInfo{W: int(x.GetWidth()), H: int(x.GetHeight()), Size: int64(x.GetFileLength()), Mime: x.GetMimetype()}
		if a := x.GetAudioMessage(); a != nil {
			p.Info.Sound = true
			p.Info.Seconds = int(a.GetSeconds())
		}
		out.StickerMessage = x
	default:
		return nil
	}
	raw, err := proto.Marshal(out)
	if err != nil {
		return nil
	}
	p.Raw = raw
	return p
}

// mediaHash names the files of a message: the first 32 hex characters of
// sha256(chat + "|" + id).
func mediaHash(chat, id string) string {
	sum := sha256.Sum256([]byte(chat + "|" + id))
	return hex.EncodeToString(sum[:])[:32]
}

var mimeExt = map[string]string{
	"audio/ogg":                     ".ogg",
	"audio/mpeg":                    ".mp3",
	"audio/mp4":                     ".m4a",
	"audio/aac":                     ".aac",
	"audio/amr":                     ".amr",
	"audio/wav":                     ".wav",
	"audio/webm":                    ".webm",
	"image/jpeg":                    ".jpg",
	"image/png":                     ".png",
	"image/webp":                    ".webp",
	"image/gif":                     ".gif",
	"video/mp4":                     ".mp4",
	"video/webm":                    ".webm",
	"video/3gpp":                    ".3gp",
	"video/quicktime":               ".mov",
	"application/pdf":               ".pdf",
	"application/zip":               ".zip",
	"text/plain":                    ".txt",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
}

// extForMime maps a MIME type to a file extension; unknown types give ".bin".
func extForMime(m string) string {
	base, _, err := mime.ParseMediaType(m)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	}
	if ext, ok := mimeExt[base]; ok {
		return ext
	}
	return ".bin"
}

func mediaDir(dataDir string) string  { return filepath.Join(dataDir, mediaDirName) }
func thumbsDir(dataDir string) string { return filepath.Join(dataDir, mediaDirName, thumbsDirName) }

func thumbExt(kind string) string {
	if kind == KindSticker {
		return ".png"
	}
	return ".jpg"
}

// cacheFilePath is where an audio download lives: media/<h>.<ext>.
func cacheFilePath(dataDir, chat, id, mimeType string) string {
	return filepath.Join(mediaDir(dataDir), mediaHash(chat, id)+extForMime(mimeType))
}

func thumbFilePath(dataDir, chat, id, kind string) string {
	return filepath.Join(thumbsDir(dataDir), mediaHash(chat, id)+thumbExt(kind))
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// fillMedia sets Thumb when the preview exists and drops File when the saved
// file is gone (m.Media.File holds the path recorded in the database).
func fillMedia(dataDir string, m *Message) {
	if m.Media == nil {
		return
	}
	m.Media.Thumb = ""
	if p := thumbFilePath(dataDir, m.Chat, m.ID, m.Kind); fileExists(p) {
		m.Media.Thumb = p
	}
	if m.Media.File != "" && !fileExists(m.Media.File) {
		m.Media.File = ""
	}
}

// writeFileAtomic writes data with mode 0600 through a temporary file.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + partSuffix
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeThumb stores a preview image under media/thumbs/.
func writeThumb(dataDir, chat, id, kind string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := os.MkdirAll(thumbsDir(dataDir), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(thumbFilePath(dataDir, chat, id, kind), data)
}

// pruneCache deletes the oldest files (by mtime) directly inside dir until the
// total size is at most maxBytes. The file keep is never deleted; partial
// downloads and subdirectories (thumbs) are ignored.
func pruneCache(dir string, maxBytes int64, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type item struct {
		path string
		size int64
		mod  int64
	}
	var items []item
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		items = append(items, item{p, fi.Size(), fi.ModTime().UnixNano()})
		total += fi.Size()
	}
	sort.Slice(items, func(a, b int) bool { return items[a].mod < items[b].mod })
	for _, it := range items {
		if total <= maxBytes {
			break
		}
		if it.path == keep {
			continue
		}
		if err := os.Remove(it.path); err == nil {
			total -= it.size
		}
	}
	return nil
}

// keyedLock serializes work per key (one download per message at a time).
type keyedLock struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLock) Lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// defaultDownloadDir returns the user's downloads folder plus "/WhatsApp":
// XDG_DOWNLOAD_DIR from ~/.config/user-dirs.dirs, else ~/Downloads.
func defaultDownloadDir(home string) string {
	base := filepath.Join(home, "Downloads")
	if f, err := os.Open(filepath.Join(home, ".config", "user-dirs.dirs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			v, ok := strings.CutPrefix(line, "XDG_DOWNLOAD_DIR=")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			v = strings.ReplaceAll(v, "${HOME}", home)
			v = strings.ReplaceAll(v, "$HOME", home)
			if filepath.IsAbs(v) {
				base = filepath.Clean(v)
			}
		}
	}
	return filepath.Join(base, "WhatsApp")
}

const maxFileNameBytes = 120

// sanitizeFileName makes a sender-controlled name safe: no path separators,
// no "..", no control characters, at most maxFileNameBytes bytes (the
// extension is kept). It returns "" when nothing usable is left.
func sanitizeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0 || unicode.IsControl(r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.Trim(out, " .")
	if len(out) <= maxFileNameBytes {
		return out
	}
	ext := filepath.Ext(out)
	if len(ext) > 20 {
		ext = ""
	}
	stem, _ := clip(strings.TrimSuffix(out, ext), maxFileNameBytes-len(ext))
	return strings.TrimRight(stem, " .") + ext
}

// downloadName builds the file name for a media message in the downloads folder.
func downloadName(kind string, ts int64, mimeType, docName string) string {
	if kind == KindDocument {
		if n := sanitizeFileName(docName); n != "" {
			return n
		}
	}
	label := map[string]string{KindImage: "Image", KindVideo: "Video", KindSticker: "Sticker", KindDocument: "Document"}[kind]
	if label == "" {
		label = "File"
	}
	return fmt.Sprintf("WhatsApp %s %s%s", label, time.Unix(ts, 0).Format("2006-01-02 15.04.05"), extForMime(mimeType))
}

// ensureDownloadDir creates the downloads folder with mode 0755 (the daemon's
// umask is 0077, so the mode is set explicitly) when it does not exist.
func ensureDownloadDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// createUnique creates a new file (O_EXCL, never overwriting) named name in dir;
// on a collision it tries "stem (1).ext", "stem (2).ext"... The file mode is 0644.
func createUnique(dir, name string) (*os.File, string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, n)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if err := f.Chmod(0o644); err != nil {
			f.Close()
			_ = os.Remove(path)
			return nil, "", err
		}
		return f, path, nil
	}
	return nil, "", errors.New("too many files with the same name")
}

// saveUnique writes data to a new uniquely named file in dir and returns its path.
func saveUnique(dir, name string, data []byte) (string, error) {
	f, path, err := createUnique(dir, name)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// removeMessageCache deletes the cached files of a message: the preview, the
// cached download and the sticker sound. Files in the user's downloads folder
// are never touched.
func removeMessageCache(dataDir, chat, id, kind string) {
	_ = os.Remove(thumbFilePath(dataDir, chat, id, kind))
	for _, key := range []string{id, id + "#sound"} {
		matches, err := filepath.Glob(filepath.Join(mediaDir(dataDir), mediaHash(chat, key)+".*"))
		if err != nil {
			continue
		}
		for _, p := range matches {
			if fileExists(p) {
				_ = os.Remove(p)
			}
		}
	}
}
