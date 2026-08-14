package rotatinglog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

func Open(path string, maxBytes int64, maxFiles int) (*Writer, error) {
	if path == "" {
		return nil, errors.New("log path is required")
	}
	if maxBytes <= 0 {
		return nil, errors.New("log maximum size must be positive")
	}
	if maxFiles < 1 {
		return nil, errors.New("log file count must be at least one")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	writer := &Writer{path: absolute, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *Writer) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(payload)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(payload)
	w.size += int64(written)
	return written, err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	w.file = nil
	oldest := w.path + fmt.Sprintf(".%d", w.maxFiles-1)
	if w.maxFiles > 1 {
		if err := removeRegular(oldest); err != nil {
			return err
		}
		for index := w.maxFiles - 2; index >= 1; index-- {
			if err := renameRegular(
				w.path+fmt.Sprintf(".%d", index),
				w.path+fmt.Sprintf(".%d", index+1),
			); err != nil {
				return err
			}
		}
		if err := renameRegular(w.path, w.path+".1"); err != nil {
			return err
		}
	} else if err := removeRegular(w.path); err != nil {
		return err
	}
	return w.open()
}

func (w *Writer) open() error {
	if info, err := os.Lstat(w.path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("log path must be a regular file")
		}
		if info.Mode().Perm() != 0o600 {
			if err := os.Chmod(w.path, 0o600); err != nil {
				return fmt.Errorf("secure log permissions: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect log path: %w", err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect open log: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func removeRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect rotated log: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("rotated log path must be a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove rotated log: %w", err)
	}
	return nil
}

func renameRegular(source string, destination string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect log before rotation: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("log path must be a regular file")
	}
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	return nil
}

var _ io.WriteCloser = (*Writer)(nil)
