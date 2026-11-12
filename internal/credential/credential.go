// Package credential keeps the agent's credential in its state folder, readable only by the agent's user.
package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const FileName = "credential.json"

var ErrNotEnrolled = errors.New("this agent is not enrolled; run `autoheal-agent enroll` first")

type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Credential struct {
	APIURL      string    `json:"api_url"`
	Agent       Ref       `json:"agent"`
	Project     Ref       `json:"project"`
	Environment Ref       `json:"environment"`
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Stamp identifies one version of the credential file, to notice when enroll rewrites it.
type Stamp struct {
	ModTime time.Time
	Size    int64
}

func Path(stateDir string) string {
	return filepath.Join(stateDir, FileName)
}

// PermissionError means the state folder or credential file can be read by other users.
type PermissionError struct {
	Path string
	Mode fs.FileMode
	Want fs.FileMode
}

func (permissionError *PermissionError) Error() string {
	return fmt.Sprintf("%s can be read by other users (mode %04o); run: chmod %o %s",
		permissionError.Path, permissionError.Mode.Perm(), permissionError.Want, permissionError.Path)
}

// EnsureDir creates the state folder with owner-only access, or checks an existing one.
func EnsureDir(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state folder: %w", err)
	}
	return checkMode(stateDir, 0o700)
}

func checkMode(path string, want fs.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return &PermissionError{Path: path, Mode: info.Mode(), Want: want}
	}
	return nil
}

func Exists(stateDir string) bool {
	_, err := os.Stat(Path(stateDir))
	return err == nil
}

// Load reads the credential, refusing it when the folder or file is open to other users.
func Load(stateDir string) (*Credential, Stamp, error) {
	path := Path(stateDir)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Stamp{}, ErrNotEnrolled
	}
	if err != nil {
		return nil, Stamp{}, err
	}
	if err := checkMode(stateDir, 0o700); err != nil {
		return nil, Stamp{}, err
	}
	if err := checkMode(path, 0o600); err != nil {
		return nil, Stamp{}, err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, Stamp{}, err
	}
	var credential Credential
	if err := json.Unmarshal(content, &credential); err != nil {
		return nil, Stamp{}, fmt.Errorf("%s is not a valid credential file: %w", path, err)
	}
	if credential.Token == "" || credential.APIURL == "" {
		return nil, Stamp{}, fmt.Errorf("%s is missing its token or API address; enroll again", path)
	}

	return &credential, Stamp{ModTime: info.ModTime(), Size: info.Size()}, nil
}

// StampOf reads the current version of the credential file without reading the file.
func StampOf(stateDir string) (Stamp, error) {
	info, err := os.Stat(Path(stateDir))
	if err != nil {
		return Stamp{}, err
	}
	return Stamp{ModTime: info.ModTime(), Size: info.Size()}, nil
}

// Save writes the credential atomically: a crash leaves either the old file or the new one.
func Save(stateDir string, credential *Credential) (Stamp, error) {
	if err := EnsureDir(stateDir); err != nil {
		return Stamp{}, err
	}
	content, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return Stamp{}, err
	}
	if err := WriteAtomic(Path(stateDir), content); err != nil {
		return Stamp{}, fmt.Errorf("saving credential: %w", err)
	}
	return StampOf(stateDir)
}

// WriteAtomic writes content to path through a synced temporary file and a rename.
func WriteAtomic(path string, content []byte) error {
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
