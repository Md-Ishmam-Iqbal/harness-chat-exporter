package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func uniqueDestination(requested string) (string, error) {
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", err
	}
	extension := filepath.Ext(absolute)
	stem := absolute[:len(absolute)-len(extension)]
	for index := 1; index < 10000; index++ {
		candidate := absolute
		if index > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, index, extension)
		}
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not find an available output filename")
}

func publishDirect(ctx context.Context, requested string, write func(io.Writer) error) (string, error) {
	directory := filepath.Dir(requested)
	temporary, err := os.CreateTemp(directory, ".hce-output-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", err
	}
	buffered := bufio.NewWriterSize(temporary, 64*1024)
	err = write(buffered)
	if flushErr := buffered.Flush(); err == nil {
		err = flushErr
	}
	if syncErr := temporary.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for {
		destination, err := uniqueDestination(requested)
		if err != nil {
			return "", err
		}
		if err := os.Link(temporaryPath, destination); err == nil {
			if chmodErr := os.Chmod(destination, 0o600); chmodErr != nil {
				_ = os.Remove(destination)
				return "", chmodErr
			}
			return destination, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish output: %w", err)
		}
	}
}
