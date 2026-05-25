package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type LocalStorage struct {
	root string

	mu      sync.Mutex
	files   map[string]*os.File // groupSlug/yearMonth → open JSONL file
	mediaMu sync.RWMutex
	media   map[string]string // hash → relative path (first-write wins)
}

func NewLocalStorage(root string) (*LocalStorage, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create backup root: %w", err)
	}
	ls := &LocalStorage{
		root:  root,
		files: make(map[string]*os.File),
		media: make(map[string]string),
	}
	if err := ls.rehydrateMediaIndex(); err != nil {
		return nil, err
	}
	return ls, nil
}

// rehydrateMediaIndex scans the backup root once at startup so that media
// downloaded in a previous run is recognized as already-present and won't be
// re-downloaded. Dedup is keyed by the filename stem (the hash).
func (l *LocalStorage) rehydrateMediaIndex() error {
	return filepath.Walk(l.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Only files under .../media/ count.
		if filepath.Base(filepath.Dir(path)) != "media" {
			return nil
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return nil
		}
		base := filepath.Base(path)
		stem := base
		if ext := filepath.Ext(base); ext != "" {
			stem = base[:len(base)-len(ext)]
		}
		if stem == "" {
			return nil
		}
		l.media[stem] = filepath.ToSlash(rel)
		return nil
	})
}

func (l *LocalStorage) AppendMessage(groupSlug, yearMonth string, line []byte) error {
	key := groupSlug + "/" + yearMonth
	l.mu.Lock()
	defer l.mu.Unlock()

	f, ok := l.files[key]
	if !ok {
		dir := filepath.Join(l.root, groupSlug, yearMonth)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		var err error
		f, err = os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open jsonl: %w", err)
		}
		l.files[key] = f
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

func (l *LocalStorage) Exists(hash, ext string) (bool, string, error) {
	l.mediaMu.RLock()
	defer l.mediaMu.RUnlock()
	if p, ok := l.media[hash]; ok {
		return true, p, nil
	}
	return false, "", nil
}

func (l *LocalStorage) WriteMedia(groupSlug, yearMonth, hash, ext string, data []byte) (string, error) {
	l.mediaMu.Lock()
	if p, ok := l.media[hash]; ok {
		l.mediaMu.Unlock()
		return p, nil
	}
	// Reserve the slot under lock so concurrent workers don't race on the
	// same hash; release the lock for the actual disk write.
	dir := filepath.Join(l.root, groupSlug, yearMonth, "media")
	name := hash
	if ext != "" {
		name = hash + "." + ext
	}
	rel := filepath.ToSlash(filepath.Join(groupSlug, yearMonth, "media", name))
	l.media[hash] = rel
	l.mediaMu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.forgetMedia(hash)
		return "", fmt.Errorf("create media dir: %w", err)
	}
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		l.forgetMedia(hash)
		return "", fmt.Errorf("write media %s: %w", abs, err)
	}
	return rel, nil
}

func (l *LocalStorage) forgetMedia(hash string) {
	l.mediaMu.Lock()
	delete(l.media, hash)
	l.mediaMu.Unlock()
}

func (l *LocalStorage) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for k, f := range l.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(l.files, k)
	}
	return firstErr
}
