package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

const webUILogReadBudget int64 = 1 << 20 // Admin log API only; does not limit model requests or upstream responses.

// readTailLines locates the tail in reverse blocks, then concatenates in one pass; complexity is linear in scanned bytes.
// When the budget truncates, the first incomplete line is dropped so fragments of secret values that cannot be reliably redacted are not displayed.
func readTailLines(f *os.File, n int, budget int64) ([]string, bool, error) {
	if n <= 0 {
		return []string{}, false, nil
	}
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("only ordinary log files may be read")
	}
	const chunkSize int64 = 16 << 10
	pos, scanned := st.Size(), int64(0)
	var chunks [][]byte
	newlines := 0
	for pos > 0 && newlines <= n && (budget <= 0 || scanned < budget) {
		size := min(chunkSize, pos)
		if budget > 0 {
			size = min(size, budget-scanned)
		}
		pos -= size
		chunk := make([]byte, int(size))
		count, err := f.ReadAt(chunk, pos)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		chunk = chunk[:count]
		chunks = append(chunks, chunk)
		scanned += int64(count)
		newlines += bytes.Count(chunk, []byte{'\n'})
		if count != int(size) {
			return nil, false, fmt.Errorf("log file was truncated while being read; please retry")
		}
	}
	truncated := pos > 0 && budget > 0 && scanned >= budget && newlines <= n
	data := make([]byte, 0, int(scanned))
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}
	if pos > 0 {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			data = data[end+1:]
		} else {
			data = nil
		}
	}
	lines := make([]string, 0, n)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) != 0 {
			lines = append(lines, string(line))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, truncated, nil
}
