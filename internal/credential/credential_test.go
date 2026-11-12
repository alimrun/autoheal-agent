package credential

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sample() *Credential {
	return &Credential{
		APIURL:      "https://autoheal.test",
		Agent:       Ref{ID: "01AGENT", Name: "web-01"},
		Project:     Ref{ID: "01PROJECT", Name: "Shop"},
		Environment: Ref{ID: "01ENV", Name: "production"},
		Token:       "test-token-value",
		ExpiresAt:   time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := stateDir(t)
	saved, err := Save(dir, sample())
	if err != nil {
		t.Fatal(err)
	}

	loaded, stamp, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded != *sample() {
		t.Errorf("loaded %+v", loaded)
	}
	if stamp != saved {
		t.Errorf("stamps differ: %+v %+v", stamp, saved)
	}

	info, _ := os.Stat(Path(dir))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credential mode %o", info.Mode().Perm())
	}
	info, _ = os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("state folder mode %o", info.Mode().Perm())
	}
}

func TestNotEnrolled(t *testing.T) {
	_, _, err := Load(stateDir(t))
	if !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("want ErrNotEnrolled, got %v", err)
	}
}

func TestReadableFileRefused(t *testing.T) {
	dir := stateDir(t)
	if _, err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(Path(dir), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load(dir)
	var permissionError *PermissionError
	if !errors.As(err, &permissionError) {
		t.Fatalf("want a permission error, got %v", err)
	}
	if permissionError.Want != 0o600 {
		t.Errorf("should ask for 600: %v", err)
	}
}

func TestReadableFolderRefused(t *testing.T) {
	dir := stateDir(t)
	if _, err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load(dir)
	var permissionError *PermissionError
	if !errors.As(err, &permissionError) || permissionError.Path != dir {
		t.Fatalf("want a permission error for the folder, got %v", err)
	}
}

func TestCrashBeforeRenameKeepsOldFile(t *testing.T) {
	dir := stateDir(t)
	if _, err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	// A crash after writing the temporary file but before the rename.
	if err := os.WriteFile(Path(dir)+".tmp", []byte(`{"token":"half`), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := Load(dir)
	if err != nil || loaded.Token != "test-token-value" {
		t.Fatalf("old credential should survive: %v %+v", err, loaded)
	}
}

func TestRewriteIsNoticed(t *testing.T) {
	dir := stateDir(t)
	first, err := Save(dir, sample())
	if err != nil {
		t.Fatal(err)
	}

	replacement := sample()
	replacement.Token = "a-longer-replacement-token-value"
	if _, err := Save(dir, replacement); err != nil {
		t.Fatal(err)
	}
	second, err := StampOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("a rewritten credential should change the stamp")
	}
}
