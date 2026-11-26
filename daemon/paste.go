package main

import (
	"context"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"
)

const (
	maxPasteSize = 20 << 20
	pasteTimeout = 15 * time.Second
)

// pasteTypes lists the clipboard image types in order of preference.
var pasteTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// pickPasteType returns the preferred image type among the offered ones.
func pickPasteType(list string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(list, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, t := range pasteTypes {
		if have[t] {
			return t
		}
	}
	return ""
}

func (d *Daemon) cmdPaste(ctx context.Context) Response {
	bin, err := exec.LookPath("wl-paste")
	if err != nil {
		return Response{Error: "no_wl_paste"}
	}
	ctx, cancel := context.WithTimeout(ctx, pasteTimeout)
	defer cancel()
	list, err := exec.CommandContext(ctx, bin, "--list-types").Output()
	if err != nil {
		return Response{Error: "no_image"}
	}
	typ := pickPasteType(string(list))
	if typ == "" {
		return Response{Error: "no_image"}
	}
	cmd := exec.CommandContext(ctx, bin, "--no-newline", "--type", typ)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Response{Error: "paste failed"}
	}
	if err := cmd.Start(); err != nil {
		return Response{Error: "paste failed"}
	}
	data, rerr := io.ReadAll(io.LimitReader(stdout, maxPasteSize+1))
	if len(data) > maxPasteSize {
		cancel()
	}
	_ = cmd.Wait()
	switch {
	case len(data) > maxPasteSize:
		return Response{Error: "too_large"}
	case rerr != nil || len(data) == 0:
		return Response{Error: "no_image"}
	}
	dir := outboxDir(d.cfg.DataDir)
	if err := ensureOutbox(dir); err != nil {
		log.Printf("paste: outbox: %v", err)
		return Response{Error: "paste failed"}
	}
	path := outboxFile(dir, "clip", extForMime(typ))
	if err := writeFileAtomic(path, data); err != nil {
		log.Printf("paste: write: %v", err)
		return Response{Error: "paste failed"}
	}
	return Response{OK: true, Data: map[string]any{"path": path}}
}
