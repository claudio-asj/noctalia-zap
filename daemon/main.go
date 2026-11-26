// noctalia-zapd is the WhatsApp helper used by the noctalia-zap plugin.
//
//	noctalia-zapd run -data DIR      run the daemon; events go to stdout as JSON lines
//	noctalia-zapd ctl <cmd> [args]   send a command to the running daemon
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "ctl":
		fs := flag.NewFlagSet("ctl", flag.ExitOnError)
		socket := fs.String("socket", defaultSocketPath(), "control socket path")
		_ = fs.Parse(os.Args[2:])
		os.Exit(runCtl(*socket, fs.Args()))
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  noctalia-zapd run -data DIR [-socket PATH] [-history-limit N] [-download-dir DIR] [-media-cache-mb N] [-qr-terminal] [-log-level LEVEL]
  noctalia-zapd ctl [-socket PATH] <command> [args]
  noctalia-zapd version

%s
`, ctlUsage)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dataDir := fs.String("data", "", "data directory (session and local message store)")
	socket := fs.String("socket", defaultSocketPath(), "control socket path")
	historyLimit := fs.Int("history-limit", 50, "messages kept per chat from history sync and returned by default")
	downloadDir := fs.String("download-dir", "", "folder for downloaded images, videos, documents and stickers (default: XDG downloads dir + /WhatsApp)")
	mediaCacheMB := fs.Int("media-cache-mb", 500, "size limit in MiB of the audio cache")
	qrTerminal := fs.Bool("qr-terminal", false, "also print the QR code to stderr")
	logLevel := fs.String("log-level", "warn", "whatsmeow log level (debug, info, warn, error)")
	_ = fs.Parse(args)

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "run: -data is required")
		return 2
	}
	if *historyLimit <= 0 || *historyLimit > maxHistoryLimit {
		fmt.Fprintf(os.Stderr, "run: -history-limit must be between 1 and %d\n", maxHistoryLimit)
		return 2
	}
	if *mediaCacheMB <= 0 {
		fmt.Fprintln(os.Stderr, "run: -media-cache-mb must be positive")
		return 2
	}
	if *downloadDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "run: cannot find the home directory; use -download-dir")
			return 2
		}
		*downloadDir = defaultDownloadDir(home)
	}
	dlDir, err := filepath.Abs(*downloadDir)
	if err != nil {
		log.Printf("download dir: %v", err)
		return 1
	}
	// Everything the daemon creates (session keys, messages, QR) is private.
	umask(0o077)
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Printf("data dir: %v", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("data dir: %v", err)
		return 1
	}
	// MkdirAll leaves an existing directory alone; tighten it.
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("data dir: %v", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	emit := NewEmitter(os.Stdout)

	waLogger := newWALogger(*logLevel)
	store.DeviceProps.Os = proto.String("Noctalia Zap")
	container, err := sqlstore.New(ctx, "sqlite3",
		sqliteDSN(filepath.Join(dir, "session.db"), "_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"),
		waLogger.Sub("Database"))
	if err != nil {
		log.Printf("open session store: %v", err)
		emit.Status(StateError, "session_store")
		return 1
	}
	defer container.Close()

	msgs, err := OpenMsgStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		log.Printf("open message store: %v", err)
		emit.Status(StateError, "message_store")
		return 1
	}
	defer msgs.Close()

	ln, err := listenControl(*socket)
	if err != nil {
		log.Printf("control socket: %v", err)
		emit.Status(StateError, "socket")
		return 1
	}
	defer func() {
		ln.Close()
		_ = os.Remove(*socket)
	}()

	d := NewDaemon(Config{
		DataDir:      dir,
		HistoryLimit: *historyLimit,
		QRTerminal:   *qrTerminal,
		DownloadDir:  dlDir,
		MediaCacheMB: *mediaCacheMB,
		WALog:        waLogger.Sub("Client"),
	}, container, msgs, emit, cancel)

	if err := d.Start(ctx); err != nil {
		log.Printf("start: %v", err)
		emit.Status(StateError, "start_failed")
	}
	// Commands are accepted only after Start, so login/logout never race with it.
	go serveControl(ctx, ln, d.Handle)

	<-ctx.Done()
	d.Stop()
	return 0
}

func newWALogger(level string) waLog.Logger {
	lvl, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil {
		lvl = zerolog.WarnLevel
	}
	zl := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: true}).
		Level(lvl).With().Timestamp().Logger()
	return waLog.Zerolog(zl)
}
