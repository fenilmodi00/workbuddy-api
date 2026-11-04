package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// parseLogOffset parses the log focus offset carried by alerts; returns -1 when missing or invalid, meaning read from the tail.
func parseLogOffset(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

// readWebLogAround reads the log window covering the given byte offset, used for "alert → jump to that line".
// Returns the lines in the window, the target line index, and whether the target was actually located (false when the file was rotated or cleaned up).
func readWebLogAround(name string, offset int64, span int64) ([]string, int, bool, error) {
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, -1, false, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, -1, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, -1, false, err
	}
	size := info.Size()
	resolved := offset <= size
	start, end := offset-span, offset+span
	if start < 0 {
		start = 0
	}
	if end > size {
		end = size
	}
	if end-start > webUILogReadBudget {
		end = start + webUILogReadBudget
	}
	if end <= start {
		return nil, -1, resolved, nil
	}
	buf := make([]byte, end-start)
	n, readErr := f.ReadAt(buf, start)
	if readErr != nil && readErr != io.EOF {
		return nil, -1, false, readErr
	}
	buf = buf[:n]
	// Record the real start of the window's first line in the file, to convert the offset into a line index.
	cursor := start
	if start > 0 {
		if cut := strings.IndexByte(string(buf), '\n'); cut >= 0 {
			cursor += int64(cut) + 1
			buf = buf[cut+1:]
		} else {
			buf = nil
		}
	}
	lines := strings.Split(string(buf), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	focus := -1
	for i, line := range lines {
		next := cursor + int64(len(line)) + 1
		if offset >= cursor && offset < next {
			focus = i
			break
		}
		cursor = next
	}
	return lines, focus, resolved && focus >= 0, nil
}
