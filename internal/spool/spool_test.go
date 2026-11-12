package spool

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/event"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func events(count int, at time.Time) []event.Event {
	built := make([]event.Event, count)
	for index := range built {
		built[index] = event.Event{
			SchemaVersion: 1,
			EventID:       event.NewID(at),
			Type:          "health_check",
			Severity:      "info",
			OccurredAt:    event.Timestamp{Time: at},
			Message:       fmt.Sprintf("web-01:api is up %d", index),
			Body:          event.HealthCheck{Name: "web-01:api", Status: "up"},
		}
	}
	return built
}

func open(t *testing.T, dir string, maxEvents int, maxBytes int64) *Spool {
	t.Helper()
	spool, err := Open(dir, maxEvents, maxBytes, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return spool
}

func TestFIFO(t *testing.T) {
	spool := open(t, t.TempDir(), 1000, 1<<20)
	first, second := events(2, time.Now()), events(3, time.Now())
	if err := spool.Append(first); err != nil {
		t.Fatal(err)
	}
	if err := spool.Append(second); err != nil {
		t.Fatal(err)
	}
	if spool.Len() != 5 {
		t.Fatalf("len %d", spool.Len())
	}

	batch, _ := spool.Oldest()
	if len(batch.Events) != 2 || batch.Events[0].EventID != first[0].EventID {
		t.Fatalf("oldest: %+v", batch)
	}
	if err := spool.Remove(batch); err != nil {
		t.Fatal(err)
	}
	batch, _ = spool.Oldest()
	if len(batch.Events) != 3 || batch.Events[0].EventID != second[0].EventID {
		t.Fatalf("next: %+v", batch)
	}
	spool.Remove(batch)
	if batch, _ := spool.Oldest(); batch != nil || spool.Len() != 0 {
		t.Fatal("spool should be empty")
	}
}

func TestSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	spool := open(t, dir, 1000, 1<<20)
	written := events(50, time.Now())
	spool.Append(written[:25])
	spool.Append(written[25:])
	os.WriteFile(filepath.Join(dir, "00000000000000000099-1.json.tmp"), []byte("half"), 0o600)

	reopened := open(t, dir, 1000, 1<<20)
	if reopened.Len() != 50 {
		t.Fatalf("len after restart %d", reopened.Len())
	}
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000099-1.json.tmp")); !os.IsNotExist(err) {
		t.Error("half-written files are removed at start")
	}
	reopened.Append(events(1, time.Now()))
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 3 || filepath.Base(files[2]) != "00000000000000000002-1.json" {
		t.Errorf("sequence continues after restart: %v", files)
	}

	batch, _ := reopened.Oldest()
	if batch.Events[0].EventID != written[0].EventID {
		t.Error("identifiers survive a restart")
	}
}

func TestCountLimitDropsTheOldest(t *testing.T) {
	spool := open(t, t.TempDir(), 100, 1<<30)
	var firstIDs []string
	for index := 0; index < 5; index++ {
		batch := events(50, time.Now())
		if index == 0 {
			for _, built := range batch {
				firstIDs = append(firstIDs, built.EventID)
			}
		}
		if err := spool.Append(batch); err != nil {
			t.Fatal(err)
		}
	}
	if spool.Len() != 100 || spool.Dropped() != 150 {
		t.Fatalf("len %d dropped %d", spool.Len(), spool.Dropped())
	}
	oldest, _ := spool.Oldest()
	if oldest.Events[0].EventID == firstIDs[0] {
		t.Error("the oldest batch should have been dropped")
	}
}

func TestSizeLimit(t *testing.T) {
	spool := open(t, t.TempDir(), 1000000, 20000)
	for range 20 {
		spool.Append(events(10, time.Now()))
	}
	total := int64(0)
	files, _ := filepath.Glob(filepath.Join(spool.dir, "*.json"))
	for _, file := range files {
		info, _ := os.Stat(file)
		total += info.Size()
	}
	if total > 20000 || spool.Dropped() == 0 {
		t.Errorf("bytes on disk %d, dropped %d", total, spool.Dropped())
	}
}

func TestCorruptBatchIsSetAside(t *testing.T) {
	dir := t.TempDir()
	spool := open(t, dir, 1000, 1<<20)
	spool.Append(events(1, time.Now()))
	good := events(2, time.Now())
	spool.Append(good)
	os.WriteFile(filepath.Join(dir, "00000000000000000000-1.json"), []byte("{not json"), 0o600)

	batch, err := spool.Oldest()
	if err != nil || batch.Events[0].EventID != good[0].EventID {
		t.Fatalf("the corrupt batch should be skipped: %v %+v", err, batch)
	}
	if _, err := os.Stat(filepath.Join(dir, "corrupt", "00000000000000000000-1.json")); err != nil {
		t.Errorf("corrupt batch should be in corrupt/: %v", err)
	}
	if spool.Len() != 2 {
		t.Errorf("len %d", spool.Len())
	}
}

func TestQuarantineKeepsTen(t *testing.T) {
	dir := t.TempDir()
	spool := open(t, dir, 1000, 1<<20)
	for range 12 {
		spool.Append(events(1, time.Now()))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, file := range files {
		os.WriteFile(file, []byte("broken"), 0o600)
	}
	if batch, _ := spool.Oldest(); batch != nil {
		t.Fatal("every batch is corrupt")
	}
	quarantined, _ := os.ReadDir(filepath.Join(dir, "corrupt"))
	if len(quarantined) != 10 {
		t.Errorf("kept %d corrupt batches, want 10", len(quarantined))
	}
}

func TestReplaceKeepsIdentifiers(t *testing.T) {
	dir := t.TempDir()
	spool := open(t, dir, 1000, 1<<20)
	old := events(2, time.Now().Add(-30*time.Hour))
	fresh := events(3, time.Now())
	spool.Append(append(old, fresh...))

	batch, _ := spool.Oldest()
	if err := spool.Replace(batch, batch.Events[2:]); err != nil {
		t.Fatal(err)
	}
	if spool.Len() != 3 {
		t.Errorf("len %d", spool.Len())
	}

	reopened := open(t, dir, 1000, 1<<20)
	again, _ := reopened.Oldest()
	if len(again.Events) != 3 || again.Events[0].EventID != fresh[0].EventID {
		t.Errorf("rewritten batch: %+v", again)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Errorf("the old file should be gone: %v", files)
	}

	if err := reopened.Replace(again, nil); err != nil || reopened.Len() != 0 {
		t.Errorf("replacing with nothing removes the batch: %v %d", err, reopened.Len())
	}
}
