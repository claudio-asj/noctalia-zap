package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest    = "org.freedesktop.portal.Desktop"
	portalPath    = "/org/freedesktop/portal/desktop"
	pickTimeout   = 30 * time.Minute
	pickTitle     = "Noctalia Zap"
	requestIface  = "org.freedesktop.portal.Request"
	responseMatch = "Response"
)

// PickedEvent is the result of the file chooser.
type PickedEvent struct {
	Type      string `json:"type"` // "picked"
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// picker allows one file chooser at a time.
type picker struct {
	mu   sync.Mutex
	open bool
}

func (p *picker) acquire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open {
		return false
	}
	p.open = true
	return true
}

func (p *picker) release() {
	p.mu.Lock()
	p.open = false
	p.mu.Unlock()
}

// pickedPath extracts the chosen file from a portal response. Only file://
// URIs are accepted; anything else counts as no selection.
func pickedPath(code uint32, results map[string]dbus.Variant) (string, bool) {
	if code != 0 {
		return "", false
	}
	v, ok := results["uris"]
	if !ok {
		return "", false
	}
	uris, ok := v.Value().([]string)
	if !ok || len(uris) == 0 {
		return "", false
	}
	u, err := url.Parse(uris[0])
	if err != nil || u.Scheme != "file" || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return "", false
	}
	return u.Path, true
}

// cmdPick opens the portal file chooser and answers right away; the choice
// comes later as a picked event.
func (d *Daemon) cmdPick(ctx context.Context) Response {
	if !d.picker.acquire() {
		return Response{Error: "busy"}
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		d.picker.release()
		return Response{Error: "no_portal"}
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(requestIface), dbus.WithMatchMember(responseMatch)); err != nil {
		conn.Close()
		d.picker.release()
		return Response{Error: "no_portal"}
	}
	var token [8]byte
	_, _ = rand.Read(token[:])
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant("nzap" + hex.EncodeToString(token[:])),
		"multiple":     dbus.MakeVariant(false),
	}
	var handle dbus.ObjectPath
	call := conn.Object(portalDest, portalPath).CallWithContext(ctx, "org.freedesktop.portal.FileChooser.OpenFile", 0, "", pickTitle, opts)
	if call.Err != nil || call.Store(&handle) != nil {
		conn.Close()
		d.picker.release()
		return Response{Error: "no_portal"}
	}
	go func() {
		defer d.picker.release()
		defer conn.Close()
		timer := time.NewTimer(pickTimeout)
		defer timer.Stop()
		for {
			select {
			case sig := <-signals:
				if sig == nil {
					d.emit.Emit(PickedEvent{Type: "picked", Cancelled: true})
					return
				}
				if sig.Path != handle || sig.Name != requestIface+"."+responseMatch || len(sig.Body) < 2 {
					continue
				}
				code, _ := sig.Body[0].(uint32)
				results, _ := sig.Body[1].(map[string]dbus.Variant)
				if path, ok := pickedPath(code, results); ok {
					d.emit.Emit(PickedEvent{Type: "picked", Path: path})
				} else {
					d.emit.Emit(PickedEvent{Type: "picked", Cancelled: true})
				}
				return
			case <-timer.C:
				d.emit.Emit(PickedEvent{Type: "picked", Cancelled: true})
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return Response{OK: true}
}
