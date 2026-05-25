package pipeline

import (
	"context"
	"errors"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/AylanBoscarino/wa-backup/internal/storage"
	"go.mau.fi/whatsmeow"
	"go.uber.org/zap"
)

// MediaJob carries everything the worker pool needs to download media,
// compute the dedup hash, persist the bytes and append the final JSONL line.
type MediaJob struct {
	GroupKey string
	YearMonth string
	Hash      string
	Ext       string
	Mime      string

	// Source holds the protobuf payload to feed cli.Download.
	Source whatsmeow.DownloadableMessage

	// Envelope is the JSONL row to write once the download is complete.
	// The downloader fills Envelope.Media before writing.
	Envelope *Message

	// OriginalFileName preserves the document filename when applicable.
	OriginalFileName string
}

// Downloader is a worker pool that downloads media, dedups by hash and
// writes the final JSONL entry through the writer.
type Downloader struct {
	cli     *whatsmeow.Client
	store   storage.Storage
	writer  *Writer
	log     *zap.SugaredLogger
	jobs    chan *MediaJob
	wg      sync.WaitGroup

	downloaded int64
	failed     int64
	deduped    int64
}

func NewDownloader(cli *whatsmeow.Client, store storage.Storage, w *Writer, log *zap.SugaredLogger, workers int) *Downloader {
	d := &Downloader{
		cli:    cli,
		store:  store,
		writer: w,
		log:    log,
		jobs:   make(chan *MediaJob, workers*4),
	}
	for i := 0; i < workers; i++ {
		d.wg.Add(1)
		go d.worker(i)
	}
	return d
}

func (d *Downloader) Enqueue(ctx context.Context, job *MediaJob) {
	select {
	case d.jobs <- job:
	case <-ctx.Done():
	}
}

// Stop drains in-flight jobs and waits for all workers to exit. Pair with
// a graceful-shutdown timeout in main.go.
func (d *Downloader) Stop() {
	close(d.jobs)
	d.wg.Wait()
}

func (d *Downloader) Stats() (downloaded, failed, deduped int64) {
	return atomic.LoadInt64(&d.downloaded), atomic.LoadInt64(&d.failed), atomic.LoadInt64(&d.deduped)
}

func (d *Downloader) worker(id int) {
	defer d.wg.Done()
	for job := range d.jobs {
		d.run(job)
	}
}

func (d *Downloader) run(job *MediaJob) {
	ctx := context.Background()
	logCtx := d.log.With("msg_id", job.Envelope.ID, "group", job.GroupKey, "hash", job.Hash)

	// Dedup: if we've seen this hash already, just reference it.
	if exists, relPath, err := d.store.Exists(job.Hash, job.Ext); err == nil && exists {
		atomic.AddInt64(&d.deduped, 1)
		job.Envelope.Media = &MediaRef{
			Hash: job.Hash,
			Ext:  job.Ext,
			Mime: job.Mime,
			Path: relPath,
			Name: job.OriginalFileName,
		}
		if err := d.writer.Append(job.GroupKey, job.Envelope); err != nil {
			logCtx.Errorw("append dedup'd message failed", "error", err)
		}
		return
	}

	data, err := d.cli.Download(ctx, job.Source)
	if err != nil {
		// Media expires on WhatsApp servers in hours/days. Log enough
		// metadata for the user to spot what was lost without crashing.
		atomic.AddInt64(&d.failed, 1)
		logCtx.Errorw("media download failed",
			"error", err,
			"type", job.Envelope.Type,
			"timestamp", job.Envelope.Timestamp,
		)
		// Still persist the message envelope so the text + metadata isn't lost.
		if err := d.writer.Append(job.GroupKey, job.Envelope); err != nil {
			logCtx.Errorw("append message without media failed", "error", err)
		}
		return
	}

	ext := job.Ext
	if ext == "" {
		ext = extFromMime(job.Mime)
	}
	relPath, err := d.store.WriteMedia(job.GroupKey, job.YearMonth, job.Hash, ext, data)
	if err != nil {
		atomic.AddInt64(&d.failed, 1)
		logCtx.Errorw("write media failed", "error", err)
		return
	}
	atomic.AddInt64(&d.downloaded, 1)

	job.Envelope.Media = &MediaRef{
		Hash: job.Hash,
		Ext:  ext,
		Mime: job.Mime,
		Size: int64(len(data)),
		Path: relPath,
		Name: job.OriginalFileName,
	}
	if err := d.writer.Append(job.GroupKey, job.Envelope); err != nil {
		logCtx.Errorw("append message failed", "error", err)
	}
}

// ExtFromMime returns a sensible file extension for the given MIME type.
// Falls back to "bin" when nothing matches so the file is never extensionless.
func extFromMime(mimeType string) string {
	if mimeType == "" {
		return "bin"
	}
	// Strip parameters such as "; codecs=..."
	if i := strings.Index(mimeType, ";"); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	// Override the system mime DB for the common WhatsApp types — it
	// otherwise returns alphabetically-first extensions like ".jpe" or
	// ".oga" which are technically correct but visually ugly.
	switch mimeType {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/3gpp":
		return "3gp"
	case "audio/ogg":
		return "ogg"
	case "audio/mp4":
		return "m4a"
	case "audio/mpeg":
		return "mp3"
	case "application/pdf":
		return "pdf"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return strings.TrimPrefix(exts[0], ".")
	}
	return "bin"
}

// ExtFromFilename takes a document filename and returns the extension
// without leading dot, or "" if there isn't one.
func ExtFromFilename(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimPrefix(ext, ".")
}

// ErrNoMediaKey is returned when a media message has no MediaKey to hash.
var ErrNoMediaKey = errors.New("media message has no MediaKey")
