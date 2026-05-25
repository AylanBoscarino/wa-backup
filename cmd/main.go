package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/AylanBoscarino/wa-backup/config"
	"github.com/AylanBoscarino/wa-backup/internal/client"
	"github.com/AylanBoscarino/wa-backup/internal/handler"
	"github.com/AylanBoscarino/wa-backup/internal/listgroups"
	"github.com/AylanBoscarino/wa-backup/internal/pipeline"
	"github.com/AylanBoscarino/wa-backup/internal/storage"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const shutdownTimeout = 30 * time.Second

type cliFlags struct {
	listGroups bool
	limit      int
	all        bool
	jsonOut    bool
	search     string
	sortBy     string
	reverse    bool
	noHeader   bool
}

func main() {
	flags := parseFlags()
	if err := run(flags); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() cliFlags {
	var f cliFlags
	flag.BoolVar(&f.listGroups, "list-groups", false, "list joined WhatsApp groups and exit")
	flag.IntVar(&f.limit, "limit", 10, "max groups to print (ignored when --all is set)")
	flag.BoolVar(&f.all, "all", false, "do not limit the number of groups")
	flag.BoolVar(&f.jsonOut, "json", false, "emit a JSON array instead of a table")
	flag.StringVar(&f.search, "search", "", "case-insensitive substring filter on group name")
	flag.StringVar(&f.sortBy, "sort", "recent", "sort field: recent|name|members|created")
	flag.BoolVar(&f.reverse, "reverse", false, "reverse the sort order")
	flag.BoolVar(&f.noHeader, "no-header", false, "omit the table header (auto when stdout is not a TTY)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Runs the daemon by default. Use --list-groups to print joined groups instead.")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()
	return f
}

func run(flags cliFlags) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := newLogger(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer log.Sync() //nolint:errcheck
	sugar := log.Sugar()

	if flags.listGroups {
		return runListGroups(cfg, sugar, flags)
	}

	sugar.Infow("starting wa-backup",
		"backup_path", cfg.BackupPath,
		"session_db", cfg.SessionDBPath,
		"media_workers", cfg.MediaWorkers,
		"allowlist", cfg.GroupAllowlist,
		"denylist", cfg.GroupDenylist,
		"history_sync_idle", cfg.HistorySyncIdle,
	)
	logFilterSummary(sugar, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForSignal(cancel, sugar)

	store, err := storage.NewLocalStorage(cfg.BackupPath)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	waClient, err := client.New(ctx, cfg.SessionDBPath, sugar)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("init whatsapp client: %w", err)
	}

	writer := pipeline.NewWriter(store)
	downloader := pipeline.NewDownloader(waClient.Underlying(), store, writer, sugar.With("component", "downloader"), cfg.MediaWorkers)
	filter := pipeline.NewGroupFilter(cfg.GroupAllowlist, cfg.GroupDenylist)
	processor := pipeline.NewProcessor(waClient.Underlying(), writer, downloader, filter, sugar.With("component", "processor"))

	msgHandler := handler.NewMessageHandler(processor, sugar.With("component", "msg-handler"))
	histHandler := handler.NewHistoryHandler(processor, cfg.HistorySyncIdle, sugar.With("component", "history-handler"))

	waClient.SetHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			msgHandler.Handle(ctx, v)
		case *events.HistorySync:
			histHandler.Handle(ctx, v)
		}
	})

	// Start the history-sync watcher in the background; it logs the
	// summary the moment the phone stops pushing batches.
	var histWG sync.WaitGroup
	histWG.Add(1)
	go func() {
		defer histWG.Done()
		histHandler.WaitForCompletion(ctx)
	}()

	runErr := waClient.Run(ctx)

	// Shutdown sequence (graceful, in SPEC order).
	shutdownStart := time.Now()
	sugar.Infow("shutdown initiated", "reason", errReason(runErr))

	// 1. Stop accepting new events.
	waClient.Shutdown()

	// 2. Drain the media worker pool, bounded by shutdownTimeout.
	drained := make(chan struct{})
	go func() {
		downloader.Stop()
		close(drained)
	}()
	select {
	case <-drained:
		sugar.Infow("media workers drained", "elapsed", time.Since(shutdownStart))
	case <-time.After(shutdownTimeout):
		sugar.Warnw("media worker drain timed out", "timeout", shutdownTimeout)
	}

	// 3. Close storage (flush JSONL handles).
	if err := store.Close(); err != nil {
		sugar.Errorw("storage close failed", "error", err)
	}

	// 4. Stop the history watcher (it watches ctx).
	cancel()
	histWG.Wait()

	// 5. Summary.
	downloaded, failed, deduped := downloader.Stats()
	sugar.Infow("shutdown complete",
		"messages_processed", processor.Stats(),
		"history_messages", histHandler.Total(),
		"media_downloaded", downloaded,
		"media_deduped", deduped,
		"media_failed", failed,
		"elapsed", time.Since(shutdownStart),
	)

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

func runListGroups(cfg *config.Config, log *zap.SugaredLogger, flags cliFlags) error {
	sortKey, err := listgroups.ParseSortKey(flags.sortBy)
	if err != nil {
		return err
	}
	noHeader := flags.noHeader
	if !isTerminal(os.Stdout) {
		// Auto-strip the header so piped output (jq, awk, fzf) stays clean.
		noHeader = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := listgroups.Options{
		Limit:    flags.limit,
		All:      flags.all,
		JSON:     flags.jsonOut,
		Search:   flags.search,
		SortBy:   sortKey,
		Reverse:  flags.reverse,
		NoHeader: noHeader,
	}
	return listgroups.Run(ctx, cfg, log, opts, os.Stdout)
}

func waitForSignal(cancel context.CancelFunc, log *zap.SugaredLogger) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	sig := <-c
	log.Infow("signal received, shutting down", "signal", sig.String())
	cancel()
}

func errReason(err error) string {
	if err == nil {
		return "context canceled"
	}
	return err.Error()
}

func logFilterSummary(log *zap.SugaredLogger, cfg *config.Config) {
	switch {
	case len(cfg.GroupAllowlist) > 0:
		log.Infow("monitoring allowlisted groups only", "count", len(cfg.GroupAllowlist))
	case len(cfg.GroupDenylist) > 0:
		log.Infow("monitoring all groups except denylisted", "denied", len(cfg.GroupDenylist))
	default:
		log.Warnw("no group filter configured — saving ALL groups (high volume!)")
	}
}

func newLogger(level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	switch level {
	case "debug":
		lvl = zapcore.DebugLevel
	case "info":
		lvl = zapcore.InfoLevel
	case "warn":
		lvl = zapcore.WarnLevel
	case "error":
		lvl = zapcore.ErrorLevel
	default:
		lvl = zapcore.InfoLevel
	}

	// Console (colored) when stderr is a TTY in dev; JSON otherwise.
	if isTerminal(os.Stderr) {
		cfg := zap.NewDevelopmentConfig()
		cfg.Level = zap.NewAtomicLevelAt(lvl)
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		return cfg.Build()
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	return cfg.Build()
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
