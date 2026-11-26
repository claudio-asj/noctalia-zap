package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	outboxDirName = "outbox"
	// outboxMaxAge is how long a leftover temporary file is kept.
	outboxMaxAge = 24 * time.Hour
)

// outboxDir holds files the daemon creates for sending (pasted images,
// recordings). It is private to the user.
func outboxDir(dataDir string) string { return filepath.Join(dataDir, outboxDirName) }

// ensureOutbox creates the outbox with mode 0700.
func ensureOutbox(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// outboxFile returns the path of a new outbox file named <prefix>-<unix ms><ext>.
func outboxFile(dir, prefix, ext string) string {
	return filepath.Join(dir, prefix+"-"+strconv.FormatInt(time.Now().UnixMilli(), 10)+ext)
}

// inOutbox reports whether path is a file directly inside the outbox.
func inOutbox(dir, path string) bool {
	return filepath.Dir(filepath.Clean(path)) == filepath.Clean(dir)
}

// cleanOutbox removes regular files older than maxAge. A missing directory is fine.
func cleanOutbox(dir string, maxAge time.Duration, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if now.Sub(fi.ModTime()) > maxAge {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
