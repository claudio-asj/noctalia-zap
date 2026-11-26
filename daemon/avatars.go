package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	avatarRecheck  = 24 * time.Hour // how often a picture is checked again
	avatarMaxBytes = 1 << 20
)

func avatarsDir(dataDir string) string { return filepath.Join(mediaDir(dataDir), "avatars") }

// avatarPath is where the picture preview of a chat is kept.
func avatarPath(dataDir, jid string) string {
	return filepath.Join(avatarsDir(dataDir), mediaHash(jid, "avatar")+".jpg")
}

// cmdAvatar refreshes the picture of one chat when it is due (the panel asks
// when a chat is opened). Answers the current path, if any.
func (d *Daemon) cmdAvatar(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	key := jid.String()
	path := avatarPath(d.cfg.DataDir, key)
	picID, checked := d.store.AvatarState(ctx, key)
	due := time.Since(time.Unix(checked, 0)) >= avatarRecheck || (picID != "" && !fileExists(path))
	if due {
		if _, _, err := d.connectedClient(); err != nil {
			return fail(err)
		}
		if d.refreshAvatar(ctx, key, picID, path) {
			d.scheduleChats()
		}
	}
	data := map[string]any{}
	if fileExists(path) {
		data["path"] = path
	}
	return Response{OK: true, Data: data}
}

// refreshAvatar updates one picture. Reports whether the file changed.
func (d *Daemon) refreshAvatar(ctx context.Context, chat, picID, path string) bool {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return false
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	existing := ""
	if fileExists(path) {
		existing = picID
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := c.GetProfilePictureInfo(rctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true, ExistingID: existing})
	switch {
	case errors.Is(err, whatsmeow.ErrProfilePictureNotSet), errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized):
		_ = d.store.SetAvatar(ctx, chat, "", now)
		return os.Remove(path) == nil
	case err != nil:
		// Network or rate limit: try again on a later pass.
		return false
	case info == nil:
		// Unchanged since ExistingID.
		_ = d.store.SetAvatar(ctx, chat, picID, now)
		return false
	}
	data, err := fetchAvatar(rctx, info.URL)
	if err != nil {
		log.Printf("avatar download: %v", err)
		return false
	}
	if err := os.MkdirAll(avatarsDir(d.cfg.DataDir), 0o700); err != nil {
		return false
	}
	if err := writeFileAtomic(path, data); err != nil {
		return false
	}
	_ = d.store.SetAvatar(ctx, chat, info.ID, now)
	return true
}

func fetchAvatar(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > avatarMaxBytes {
		return nil, errors.New("too large")
	}
	return data, nil
}
