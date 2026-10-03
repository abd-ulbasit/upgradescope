package mcp

import (
	"fmt"
	"io"
	"os"
)

// ReadFile reads the regular file at path, refusing anything else and any
// file larger than max bytes. A path a tool reads comes from an assistant,
// which prompt-injected content can steer, so it may name what is not a
// file at all: a FIFO, on which a plain open blocks until a writer comes
// (forever, a goroutine per call); /dev/stdin, which on the stdio
// transport would read the client's JSON-RPC frames; a device that never
// ends. So the path is stat'ed first and only a regular file within the
// bound is opened, without blocking where the platform allows it, and the
// opened file is checked again to be that same regular file before it is
// read, through the bound. The error never quotes the file's contents.
func ReadFile(path string, max int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := regularWithin(path, fi, max); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, opened) {
		return nil, fmt.Errorf("%s changed while it was being opened", path)
	}
	if err := regularWithin(path, opened, max); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(raw)) > max {
		return nil, tooLarge(path, max)
	}
	return raw, nil
}

func regularWithin(path string, fi os.FileInfo, max int64) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (%s): a tool reads only a report or inventory file", path, fileKind(fi.Mode()))
	}
	if fi.Size() > max {
		return tooLarge(path, max)
	}
	return nil
}

func fileKind(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&os.ModeNamedPipe != 0:
		return "a named pipe"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeCharDevice != 0:
		return "a character device"
	case m&os.ModeDevice != 0:
		return "a device"
	default:
		return "not a plain file"
	}
}

func tooLarge(path string, max int64) error {
	return fmt.Errorf("%s is larger than %s, the most a tool reads", path, mib(max))
}

// mib renders n bytes in MiB, as a whole number when it is one.
func mib(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
