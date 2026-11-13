package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seenTaskAt(id, nonce string, expiresAt time.Time) *Task {
	return &Task{ID: id, Nonce: nonce, ExpiresAt: expiresAt}
}

func TestReplayStoreSurvivesARestart(t *testing.T) {
	stateDir := t.TempDir()
	store := openStore(t, stateDir)
	if store.Seen(testTaskID, testNonce) {
		t.Fatal("an empty store has seen nothing")
	}
	if err := store.Remember(seenTaskAt(testTaskID, testNonce, testNow.Add(5*time.Minute)), testNow); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, stateDir)
	if !reopened.Seen(testTaskID, "ffffffffffffffffffffffffffffffff") {
		t.Error("the task ID was forgotten across a restart")
	}
	if !reopened.Seen("01JCTASK000000000000000009", testNonce) {
		t.Error("the nonce was forgotten across a restart")
	}
	if reopened.Seen("01JCTASK000000000000000009", "ffffffffffffffffffffffffffffffff") {
		t.Error("an unknown task counts as seen")
	}
}

func TestReplayStoreFileIsPrivateAndLeavesNoTemporaryFile(t *testing.T) {
	stateDir := t.TempDir()
	store := openStore(t, stateDir)
	if err := store.Remember(seenTaskAt(testTaskID, testNonce, testNow), testNow); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(stateDir, ReplayFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(stateDir)
	if len(entries) != 1 {
		t.Errorf("the state folder should hold only the store: %v", entries)
	}
}

func TestReplayStorePrunesAnHourAfterExpiry(t *testing.T) {
	stateDir := t.TempDir()
	store := openStore(t, stateDir)
	expiry := testNow.Add(5 * time.Minute)
	if err := store.Remember(seenTaskAt(testTaskID, testNonce, expiry), testNow); err != nil {
		t.Fatal(err)
	}

	second := seenTaskAt("01JCTASK000000000000000002", "11111111111111111111111111111111", expiry.Add(time.Hour))
	if err := store.Remember(second, expiry.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !store.Seen(testTaskID, "") {
		t.Error("a task must be remembered until an hour after its expiry")
	}

	third := seenTaskAt("01JCTASK000000000000000003", "22222222222222222222222222222222", expiry.Add(2*time.Hour))
	if err := store.Remember(third, expiry.Add(time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	if store.Seen(testTaskID, "") {
		t.Error("a task is pruned once an hour has passed since its expiry")
	}
	reopened := openStore(t, stateDir)
	if reopened.Seen(testTaskID, "") || !reopened.Seen(second.ID, "") || !reopened.Seen(third.ID, "") {
		t.Error("the pruned store on disk should hold the two later tasks only")
	}
}

func TestReplayStoreKeepsItsMemoryWhenTheWriteFails(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, stateDir)
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}

	if err := store.Remember(seenTaskAt(testTaskID, testNonce, testNow), testNow); err == nil {
		t.Fatal("a store that cannot be written must say so, so the task is never acted on")
	}
	if store.Seen(testTaskID, testNonce) {
		t.Error("a task that was not saved must not count as remembered")
	}
}

func TestReplayStoreRefusesACorruptFile(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, ReplayFileName), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenReplayStore(stateDir)
	if err == nil || !strings.Contains(err.Error(), "not a valid replay store") {
		t.Errorf("unexpected error: %v", err)
	}
}
