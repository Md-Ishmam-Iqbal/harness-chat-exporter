package pipeline

import (
	"errors"
	"os"
	"path/filepath"
)

type Workspace struct {
	path   string
	closed bool
}

func NewWorkspace(parent string) (*Workspace, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	path, err := os.MkdirTemp(parent, "hce-work-")
	if err != nil {
		return nil, errors.New("create export workspace")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, errors.New("secure export workspace")
	}
	return &Workspace{path: path}, nil
}

func (w *Workspace) Path() string { return w.path }

func (w *Workspace) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	if err := os.RemoveAll(w.path); err != nil {
		return errors.New("remove export workspace")
	}
	return nil
}

func (w *Workspace) newSpool() (*os.File, string, error) {
	if w == nil || w.closed {
		return nil, "", errors.New("export workspace is closed")
	}
	file, err := os.CreateTemp(w.path, "session-*.jsonl")
	if err != nil {
		return nil, "", errors.New("create session spool")
	}
	path := file.Name()
	if filepath.Dir(path) != filepath.Clean(w.path) {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", errors.New("invalid session spool location")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", errors.New("secure session spool")
	}
	return file, path, nil
}
