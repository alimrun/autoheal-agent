package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/alimrun/autoheal-agent/internal/credential"
)

const (
	ReplayFileName = "recovery-seen.json"
	// ReplayRetention is how long a task is remembered after it expired.
	ReplayRetention = time.Hour
)

type seenTask struct {
	TaskID    string    `json:"task_id"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReplayStore remembers every task received, on disk, so a restart never forgets one.
type ReplayStore struct {
	path string

	mu   sync.Mutex
	seen []seenTask
}

// OpenReplayStore reads the store in stateDir. A missing file is an empty store.
func OpenReplayStore(stateDir string) (*ReplayStore, error) {
	store := &ReplayStore{path: filepath.Join(stateDir, ReplayFileName)}
	content, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &store.seen); err != nil {
		return nil, fmt.Errorf("%s is not a valid replay store: %w", store.path, err)
	}
	return store, nil
}

// Seen reports whether the task ID or the nonce was received before.
func (store *ReplayStore) Seen(taskID, nonce string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return slices.ContainsFunc(store.seen, func(known seenTask) bool {
		return known.TaskID == taskID || known.Nonce == nonce
	})
}

// Remember records the task and drops entries past their retention. It returns only once the
// file is safely on disk.
func (store *ReplayStore) Remember(task *Task, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	kept := slices.DeleteFunc(slices.Clone(store.seen), func(known seenTask) bool {
		return now.After(known.ExpiresAt.Add(ReplayRetention))
	})
	kept = append(kept, seenTask{TaskID: task.ID, Nonce: task.Nonce, ExpiresAt: task.ExpiresAt.UTC()})

	content, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if err := credential.WriteAtomic(store.path, content); err != nil {
		return fmt.Errorf("saving replay store: %w", err)
	}
	store.seen = kept
	return nil
}
