package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"rsc.io/qr"
)

// Client wraps whatsmeow.Client and owns the reconnection loop.
//
// Reconnect policy (per SPEC):
//   - Disconnected (transient): exponential backoff 1s → 2s → 4s → … capped at 5 min
//   - TemporaryBan: wait the duration WhatsApp returned, fall back to ~1 h
//   - LoggedOut / StreamReplaced: terminal — caller must re-pair manually
type Client struct {
	cli       *whatsmeow.Client
	container *sqlstore.Container
	log       *zap.SugaredLogger

	handler whatsmeow.EventHandler

	disconnectCh chan struct{}
	fatalCh      chan error
	banCh        chan time.Duration
}

const (
	backoffInitial = 1 * time.Second
	backoffMax     = 5 * time.Minute
	banFallback    = 1 * time.Hour
)

func New(ctx context.Context, sessionDBPath string, log *zap.SugaredLogger) (*Client, error) {
	wlog := newWaLogAdapter(log)

	// Make sure the directory that will hold wa-session.db exists with
	// owner-only permission BEFORE the DB file is created. Even if the
	// driver creates the file world-readable (it does — modernc/sqlite
	// honours umask only), a 0700 parent already blocks other users.
	if dir := filepath.Dir(sessionDBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create session dir: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", sessionDBPath)
	container, err := sqlstore.New(ctx, "sqlite", dsn, wlog.Sub("DB"))
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	// Belt-and-suspenders: tighten the DB file itself. Ignore ENOENT
	// because some drivers create it lazily on first write.
	if err := chmodIfExists(sessionDBPath, 0o600); err != nil {
		log.Warnw("could not tighten session DB permissions", "path", sessionDBPath, "error", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}
	cli := whatsmeow.NewClient(device, wlog.Sub("Client"))
	// We drive reconnect ourselves so we can satisfy the SPEC's
	// exponential-backoff and ban-aware reconnection rules.
	cli.EnableAutoReconnect = false

	c := &Client{
		cli:          cli,
		container:    container,
		log:          log,
		disconnectCh: make(chan struct{}, 8),
		fatalCh:      make(chan error, 1),
		banCh:        make(chan time.Duration, 1),
	}
	cli.AddEventHandler(c.dispatch)

	// Tighten one more time now that GetFirstDevice has definitely touched
	// the DB. Walk the sqlite sidecar files too (-wal/-shm) — they hold
	// the same crypto material.
	tightenSessionFiles(sessionDBPath, log)

	return c, nil
}

func tightenSessionFiles(dbPath string, log *zap.SugaredLogger) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := chmodIfExists(dbPath+suffix, 0o600); err != nil {
			log.Warnw("chmod session file failed", "path", dbPath+suffix, "error", err)
		}
	}
}

// Underlying returns the raw whatsmeow.Client. The pipeline uses it for
// media downloads and group metadata lookups.
func (c *Client) Underlying() *whatsmeow.Client { return c.cli }

// SetHandler registers the user-supplied event handler. It is called for
// every event after the reconnect interceptor inspects it.
func (c *Client) SetHandler(h whatsmeow.EventHandler) { c.handler = h }

func (c *Client) dispatch(evt interface{}) {
	switch v := evt.(type) {
	case *events.LoggedOut:
		select {
		case c.fatalCh <- fmt.Errorf("logged out from WhatsApp (reason: %v); delete the session DB and re-scan QR", v.Reason):
		default:
		}
	case *events.StreamReplaced:
		select {
		case c.fatalCh <- errors.New("another device connected with the same session; this instance was kicked"):
		default:
		}
	case *events.TemporaryBan:
		c.log.Warnw("temporary ban", "code", v.Code.String(), "expire", v.Expire)
		select {
		case c.banCh <- v.Expire:
		default:
		}
	case *events.Disconnected:
		c.log.Warnw("disconnected from WhatsApp")
		select {
		case c.disconnectCh <- struct{}{}:
		default:
		}
	case *events.Connected:
		c.log.Infow("connected to WhatsApp")
	}
	if c.handler != nil {
		c.handler(evt)
	}
}

// PairIfNeeded performs the QR pairing flow when no session exists; for
// already-paired sessions it just establishes a quick connection. Used by
// the setup wizard and by list-groups when they need to talk to the server
// once without entering the full daemon Run loop.
func (c *Client) PairIfNeeded(ctx context.Context) error {
	return c.initialConnect(ctx)
}

// IsPaired reports whether the session DB has a usable device. Lets
// callers decide whether to skip the QR step.
func (c *Client) IsPaired() bool {
	return c.cli.Store.ID != nil
}

// Run blocks until ctx is canceled or a fatal disconnect happens. It
// performs the initial connection (including QR display for new devices)
// and reconnects with exponential backoff after transient drops.
func (c *Client) Run(ctx context.Context) error {
	if err := c.initialConnect(ctx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-c.fatalCh:
			return err
		case wait := <-c.banCh:
			if wait <= 0 {
				wait = banFallback
			}
			c.log.Warnw("waiting out temporary ban", "duration", wait)
			if !sleepCtx(ctx, wait) {
				return ctx.Err()
			}
			if err := c.reconnectWithBackoff(ctx); err != nil {
				return err
			}
		case <-c.disconnectCh:
			if err := c.reconnectWithBackoff(ctx); err != nil {
				return err
			}
		}
	}
}

func (c *Client) initialConnect(ctx context.Context) error {
	if c.cli.Store.ID != nil {
		c.log.Infow("session found, connecting", "jid", c.cli.Store.ID.String())
		return c.reconnectWithBackoff(ctx)
	}
	c.log.Infow("no session found, starting QR pairing flow")
	qrChan, err := c.cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("get qr channel: %w", err)
	}
	if err := c.cli.Connect(); err != nil {
		return fmt.Errorf("connect for qr: %w", err)
	}
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			renderQR(os.Stderr, evt.Code, evt.Timeout)
		case "success":
			fmt.Fprintln(os.Stderr, "\n✓ Paired successfully. Session saved.")
			c.log.Infow("QR scan successful, session saved")
			// WhatsApp tears down the anonymous pairing websocket right
			// after PairSuccess. Force a clean local disconnect so we
			// know the state is settled, then reconnect as the
			// authenticated device. Without this, our caller's
			// WaitForConnection races with the server-initiated close.
			c.cli.Disconnect()
			fmt.Fprintln(os.Stderr, "Reconnecting as authenticated device…")
			return c.reconnectWithBackoff(ctx)
		case "timeout":
			return errors.New("QR scan timed out before user scanned (5 minutes elapsed)")
		case "err-client-outdated":
			return errors.New("client outdated; update whatsmeow")
		case "err-scanned-without-multidevice":
			return errors.New("scanned without multidevice enabled")
		default:
			if evt.Error != nil {
				return fmt.Errorf("qr error: %w", evt.Error)
			}
			c.log.Debugw("qr event", "event", evt.Event)
		}
	}
	return errors.New("QR channel closed unexpectedly")
}

// renderQR clears the screen and prints a single, current QR. WhatsApp
// rotates the code roughly every 20 seconds for security; if we just
// appended each new code to scrollback the user might scan an expired
// one and see "verify your connection" on their phone. HalfBlock format
// gives denser, higher-contrast output that scans more reliably than
// the full-block default.
func renderQR(w *os.File, code string, expiresIn time.Duration) {
	// \x1b[2J = clear screen, \x1b[H = move cursor to top-left.
	fmt.Fprint(w, "\x1b[2J\x1b[H")
	fmt.Fprintln(w, "Pair this device with WhatsApp")
	fmt.Fprintln(w, "──────────────────────────────")
	fmt.Fprintln(w, "On your phone:  WhatsApp → Settings → Linked devices → Link a device")
	fmt.Fprintln(w)
	if expiresIn > 0 {
		fmt.Fprintf(w, "  ↻  This code expires in ~%s. A fresh one will appear automatically.\n", expiresIn.Round(time.Second))
		fmt.Fprintln(w, "     Scan the QR below — older codes (if you scroll up) are stale.")
	}
	fmt.Fprintln(w)
	qrterminal.GenerateHalfBlock(code, qr.L, w)
}

func (c *Client) reconnectWithBackoff(ctx context.Context) error {
	delay := backoffInitial
	for {
		if c.cli.IsConnected() {
			return nil
		}
		err := c.cli.Connect()
		if err == nil || errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return nil
		}
		c.log.Warnw("reconnect failed", "error", err, "next_delay", delay)
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
		delay *= 2
		if delay > backoffMax {
			delay = backoffMax
		}
	}
}

// Shutdown disconnects gracefully. The container is left open because
// closing it would invalidate the cached session for the next run.
func (c *Client) Shutdown() {
	c.cli.Disconnect()
}

// chmodIfExists tightens mode on the given path. Missing files (e.g. the
// session DB before first write) are treated as success.
func chmodIfExists(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waLogAdapter bridges whatsmeow's waLog.Logger to zap.
type waLogAdapter struct {
	log    *zap.SugaredLogger
	module string
}

func newWaLogAdapter(log *zap.SugaredLogger) waLog.Logger {
	return &waLogAdapter{log: log, module: "whatsmeow"}
}

func (a *waLogAdapter) Errorf(msg string, args ...interface{}) {
	a.log.Errorw(fmt.Sprintf(msg, args...), "module", a.module)
}
func (a *waLogAdapter) Warnf(msg string, args ...interface{}) {
	a.log.Warnw(fmt.Sprintf(msg, args...), "module", a.module)
}
func (a *waLogAdapter) Infof(msg string, args ...interface{}) {
	a.log.Infow(fmt.Sprintf(msg, args...), "module", a.module)
}
func (a *waLogAdapter) Debugf(msg string, args ...interface{}) {
	a.log.Debugw(fmt.Sprintf(msg, args...), "module", a.module)
}
func (a *waLogAdapter) Sub(mod string) waLog.Logger {
	return &waLogAdapter{log: a.log, module: a.module + "/" + mod}
}
