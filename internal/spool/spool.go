// Package spool keeps batches of events on disk until the platform accepts them.
package spool

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/alimrun/autoheal-agent/internal/credential"
	"github.com/alimrun/autoheal-agent/internal/event"
)

const maxQuarantined = 10

type entry struct {
	sequence uint64
	count    int
	size     int64
}

func (entry entry) name() string {
	return fmt.Sprintf("%020d-%d.json", entry.sequence, entry.count)
}

// Batch is the oldest waiting batch, as read from disk.
type Batch struct {
	Events []event.Event
	entry  entry
}

type Spool struct {
	dir       string
	maxEvents int
	maxBytes  int64
	log       *slog.Logger

	mu      sync.Mutex
	entries []entry
	next    uint64
	events  int
	bytes   int64
	dropped int
	pruning bool
}

// Open prepares dir, removing half-written files and reading the waiting batches' names.
func Open(dir string, maxEvents int, maxBytes int64, log *slog.Logger) (*Spool, error) {
	if err := os.MkdirAll(filepath.Join(dir, "corrupt"), 0o700); err != nil {
		return nil, fmt.Errorf("creating spool: %w", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	spool := &Spool{dir: dir, maxEvents: maxEvents, maxBytes: maxBytes, log: log}
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		parsed, ok := parseName(name)
		if !ok {
			continue
		}
		info, err := file.Info()
		if err != nil {
			continue
		}
		parsed.size = info.Size()
		spool.entries = append(spool.entries, parsed)
	}

	sort.Slice(spool.entries, func(left, right int) bool {
		return spool.entries[left].sequence < spool.entries[right].sequence
	})
	for _, waiting := range spool.entries {
		spool.events += waiting.count
		spool.bytes += waiting.size
		spool.next = max(spool.next, waiting.sequence+1)
	}
	return spool, nil
}

func parseName(name string) (entry, bool) {
	base, found := strings.CutSuffix(name, ".json")
	if !found {
		return entry{}, false
	}
	sequenceText, countText, found := strings.Cut(base, "-")
	if !found {
		return entry{}, false
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil {
		return entry{}, false
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 0 {
		return entry{}, false
	}
	return entry{sequence: sequence, count: count}, true
}

// Append writes events as one new batch, first dropping the oldest batches if the spool would overflow.
func (spool *Spool) Append(events []event.Event) error {
	if len(events) == 0 {
		return nil
	}
	content, err := json.Marshal(events)
	if err != nil {
		return err
	}

	spool.mu.Lock()
	defer spool.mu.Unlock()

	dropped := 0
	for len(spool.entries) > 0 && (spool.events+len(events) > spool.maxEvents || spool.bytes+int64(len(content)) > spool.maxBytes) {
		oldest := spool.entries[0]
		os.Remove(filepath.Join(spool.dir, oldest.name()))
		spool.forget(0)
		dropped += oldest.count
	}
	if dropped > 0 {
		spool.dropped += dropped
		if !spool.pruning {
			spool.log.Warn("spool full: dropping the oldest events", "dropped", dropped, "limit_events", spool.maxEvents)
		}
		spool.pruning = true
	} else if spool.pruning && spool.events+len(events) < spool.maxEvents/2 {
		spool.pruning = false
	}

	written := entry{sequence: spool.next, count: len(events), size: int64(len(content))}
	if err := credential.WriteAtomic(filepath.Join(spool.dir, written.name()), content); err != nil {
		return fmt.Errorf("writing spool batch: %w", err)
	}
	spool.next++
	spool.entries = append(spool.entries, written)
	spool.events += written.count
	spool.bytes += written.size
	return nil
}

func (spool *Spool) forget(index int) {
	spool.events -= spool.entries[index].count
	spool.bytes -= spool.entries[index].size
	spool.entries = append(spool.entries[:index], spool.entries[index+1:]...)
}

// Oldest returns the oldest batch, or nil when nothing waits. A batch that cannot be read is
// moved to corrupt/ so it never blocks the ones behind it.
func (spool *Spool) Oldest() (*Batch, error) {
	spool.mu.Lock()
	defer spool.mu.Unlock()

	for len(spool.entries) > 0 {
		oldest := spool.entries[0]
		path := filepath.Join(spool.dir, oldest.name())
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			spool.forget(0)
			continue
		}
		if err != nil {
			return nil, err
		}

		var events []event.Event
		if err := json.Unmarshal(content, &events); err != nil {
			spool.quarantine(path, oldest.name())
			spool.forget(0)
			spool.log.Error("spool batch could not be read and was set aside", "file", oldest.name(), "error", err.Error())
			continue
		}
		return &Batch{Events: events, entry: oldest}, nil
	}
	return nil, nil
}

func (spool *Spool) quarantine(path, name string) {
	corruptDir := filepath.Join(spool.dir, "corrupt")
	os.Rename(path, filepath.Join(corruptDir, name))

	files, err := os.ReadDir(corruptDir)
	if err != nil || len(files) <= maxQuarantined {
		return
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Name() < files[right].Name() })
	for _, file := range files[:len(files)-maxQuarantined] {
		os.Remove(filepath.Join(corruptDir, file.Name()))
	}
}

// Remove deletes a batch once the platform has accepted or refused it for good.
func (spool *Spool) Remove(batch *Batch) error {
	spool.mu.Lock()
	defer spool.mu.Unlock()

	if err := os.Remove(filepath.Join(spool.dir, batch.entry.name())); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index, waiting := range spool.entries {
		if waiting.sequence == batch.entry.sequence {
			spool.forget(index)
			break
		}
	}
	return nil
}

// Replace rewrites a batch with only kept, keeping its place and its events' identifiers.
func (spool *Spool) Replace(batch *Batch, kept []event.Event) error {
	if len(kept) == 0 {
		return spool.Remove(batch)
	}
	content, err := json.Marshal(kept)
	if err != nil {
		return err
	}

	spool.mu.Lock()
	defer spool.mu.Unlock()

	replacement := entry{sequence: batch.entry.sequence, count: len(kept), size: int64(len(content))}
	if err := credential.WriteAtomic(filepath.Join(spool.dir, replacement.name()), content); err != nil {
		return err
	}
	if replacement.name() != batch.entry.name() {
		os.Remove(filepath.Join(spool.dir, batch.entry.name()))
	}
	for index, waiting := range spool.entries {
		if waiting.sequence == batch.entry.sequence {
			spool.events += replacement.count - waiting.count
			spool.bytes += replacement.size - waiting.size
			spool.entries[index] = replacement
			break
		}
	}
	batch.entry = replacement
	batch.Events = kept
	return nil
}

// Len is the number of events waiting.
func (spool *Spool) Len() int {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	return spool.events
}

// Dropped is how many events were dropped because the spool was full.
func (spool *Spool) Dropped() int {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	return spool.dropped
}
