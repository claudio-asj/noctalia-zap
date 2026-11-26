package main

import (
	"errors"
	"os/exec"
	"sync"
	"time"
)

var errNoPlayer = errors.New("no_player")

// playerArgv returns the fixed command line used to play a file, or nil when
// no player is installed. The path is always a separate argv element.
func playerArgv(path string) []string {
	if p, err := exec.LookPath("mpv"); err == nil {
		return []string{p, "--no-video", "--really-quiet", "--no-terminal", path}
	}
	if p, err := exec.LookPath("ffplay"); err == nil {
		return []string{p, "-nodisp", "-autoexit", "-loglevel", "quiet", path}
	}
	return nil
}

type playback struct {
	cmd  *exec.Cmd
	chat string
	id   string
	done chan struct{}
}

// Player runs at most one audio process at a time.
type Player struct {
	mu   sync.Mutex
	cur  *playback
	emit *Emitter
}

func NewPlayer(emit *Emitter) *Player { return &Player{emit: emit} }

type PlaybackEvent struct {
	Type  string `json:"type"` // "playback"
	Chat  string `json:"chat"`
	ID    string `json:"id"`
	State string `json:"state"` // "playing" or "stopped"
	// StartedMS and Seconds are set only while playing: when playback began
	// (unix ms) and the duration in seconds, left out when unknown.
	StartedMS int64 `json:"started_ms,omitempty"`
	Seconds   int   `json:"seconds,omitempty"`
}

// stopCurrent kills the running process, if any, and waits until its
// "stopped" event was emitted.
func (p *Player) stopCurrent() bool {
	p.mu.Lock()
	old := p.cur
	p.mu.Unlock()
	if old == nil {
		return false
	}
	_ = old.cmd.Process.Kill()
	<-old.done
	return true
}

// Play stops the current playback and starts a new one. seconds is the
// duration of the audio, 0 when unknown.
func (p *Player) Play(chat, id, path string, seconds int) error {
	argv := playerArgv(path)
	if argv == nil {
		return errNoPlayer
	}
	for {
		p.stopCurrent()
		p.mu.Lock()
		if p.cur != nil { // another Play won the race; stop it too
			p.mu.Unlock()
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			p.mu.Unlock()
			return err
		}
		pb := &playback{cmd: cmd, chat: chat, id: id, done: make(chan struct{})}
		p.cur = pb
		p.emit.Emit(PlaybackEvent{Type: "playback", Chat: chat, ID: id, State: "playing",
			StartedMS: time.Now().UnixMilli(), Seconds: max(seconds, 0)})
		p.mu.Unlock()
		go func() {
			_ = cmd.Wait()
			p.mu.Lock()
			if p.cur == pb {
				p.cur = nil
			}
			p.mu.Unlock()
			p.emit.Emit(PlaybackEvent{Type: "playback", Chat: chat, ID: id, State: "stopped"})
			close(pb.done)
		}()
		return nil
	}
}

// Stop kills the current playback, if any.
func (p *Player) Stop() { p.stopCurrent() }
