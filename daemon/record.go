package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxRecording = 15 * time.Minute
	stopWait     = 5 * time.Second
)

// RecordingEvent reports the state of the voice recorder.
type RecordingEvent struct {
	Type    string `json:"type"`  // "recording"
	State   string `json:"state"` // "recording" or "stopped"
	Started int64  `json:"started,omitempty"`
	// Path and Seconds are set when the recording stopped by itself at the limit.
	Path    string `json:"path,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

type recording struct {
	cmd     *exec.Cmd
	wav     string
	started time.Time
	done    chan struct{}
	timer   *time.Timer
}

// Recorder keeps at most one pw-record process.
type Recorder struct {
	mu   sync.Mutex
	cur  *recording
	emit *Emitter
	dir  string
}

func NewRecorder(emit *Emitter, dir string) *Recorder { return &Recorder{emit: emit, dir: dir} }

func (d *Daemon) cmdRecord(req Request) Response {
	r := d.recorder
	switch req.Text {
	case "start":
		if err := r.Start(); err != nil {
			return fail(err)
		}
		return Response{OK: true}
	case "stop":
		path, secs, err := r.Stop(false)
		if err != nil {
			return fail(err)
		}
		return Response{OK: true, Data: map[string]any{"path": path, "seconds": secs}}
	case "cancel":
		if err := r.Cancel(); err != nil {
			return fail(err)
		}
		return Response{OK: true}
	}
	return Response{Error: "unknown command"}
}

func (r *Recorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return errors.New("already_recording")
	}
	rec, err := exec.LookPath("pw-record")
	if err != nil {
		return errors.New("no_recorder")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("no_ffmpeg")
	}
	if err := ensureOutbox(r.dir); err != nil {
		log.Printf("record: outbox: %v", err)
		return errors.New("record failed")
	}
	wav := outboxFile(r.dir, "rec", ".wav")
	cmd := exec.Command(rec, "--rate", "48000", "--channels", "1", wav)
	if err := cmd.Start(); err != nil {
		return errors.New("no_recorder")
	}
	cur := &recording{cmd: cmd, wav: wav, started: time.Now(), done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(cur.done)
	}()
	cur.timer = time.AfterFunc(maxRecording, func() {
		if _, _, err := r.Stop(true); err != nil && err.Error() != "not_recording" {
			log.Printf("record: auto stop: %v", err)
		}
	})
	r.cur = cur
	r.emit.Emit(RecordingEvent{Type: "recording", State: "recording", Started: cur.started.Unix()})
	return nil
}

// take detaches the current recording, nil when there is none.
func (r *Recorder) take() *recording {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur
	r.cur = nil
	if cur != nil {
		cur.timer.Stop()
	}
	return cur
}

// halt ends the process: SIGINT lets pw-record finish the file, with a kill as fallback.
func (c *recording) halt(graceful bool) {
	if graceful {
		_ = c.cmd.Process.Signal(syscall.SIGINT)
	} else {
		_ = c.cmd.Process.Kill()
	}
	select {
	case <-c.done:
	case <-time.After(stopWait):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// Stop ends the recording and returns the Opus file and its length. auto is
// true when the time limit fired: the result then goes out in the event.
func (r *Recorder) Stop(auto bool) (string, int, error) {
	cur := r.take()
	if cur == nil {
		return "", 0, errors.New("not_recording")
	}
	elapsed := time.Since(cur.started)
	cur.halt(true)
	defer os.Remove(cur.wav)
	ogg := strings.TrimSuffix(cur.wav, ".wav") + ".ogg"
	path, secs, err := convertRecording(cur.wav, ogg, elapsed)
	ev := RecordingEvent{Type: "recording", State: "stopped"}
	if auto && err == nil {
		ev.Path, ev.Seconds = path, secs
	}
	r.emit.Emit(ev)
	return path, secs, err
}

func convertRecording(wav, ogg string, elapsed time.Duration) (string, int, error) {
	if fi, err := os.Stat(wav); err != nil || fi.Size() == 0 {
		return "", 0, errors.New("record failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", 0, errors.New("no_ffmpeg")
	}
	if err := exec.CommandContext(ctx, ff, "-y", "-loglevel", "error", "-i", wav, "-ac", "1", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "32k", "-application", "voip", ogg).Run(); err != nil {
		_ = os.Remove(ogg)
		log.Printf("record: convert: %v", err)
		return "", 0, errors.New("convert failed")
	}
	if err := os.Chmod(ogg, 0o600); err != nil {
		_ = os.Remove(ogg)
		return "", 0, errors.New("convert failed")
	}
	secs := probeSeconds(ctx, ogg)
	if secs == 0 {
		secs = wholeSeconds(elapsed.Seconds())
	}
	return ogg, secs, nil
}

// Cancel kills the recording and deletes the file.
func (r *Recorder) Cancel() error {
	cur := r.take()
	if cur == nil {
		return errors.New("not_recording")
	}
	cur.halt(false)
	_ = os.Remove(cur.wav)
	r.emit.Emit(RecordingEvent{Type: "recording", State: "stopped"})
	return nil
}

// Shutdown discards any recording when the daemon exits.
func (r *Recorder) Shutdown() {
	if cur := r.take(); cur != nil {
		cur.halt(false)
		_ = os.Remove(cur.wav)
	}
}
