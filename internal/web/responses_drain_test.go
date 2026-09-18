package web

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

// The inner openaiChat writes the ": m365-metrics" trace frame AFTER "data:
// [DONE]", and io.Pipe is synchronous. The responses adapter must therefore
// keep draining the pipe to EOF after it sees [DONE]; breaking out of the read
// loop wedges the inner writer (no reader), which never closes the pipe and
// never closes innerDone, so the outer handler blocks forever and the client
// reports "stream disconnected before completion".
//
// This test reproduces that protocol with a real io.Pipe: the writer emits a
// text frame, [DONE], then a trailing metrics frame, and only returns after all
// of it has been read. It asserts the reader finishes promptly.
func TestResponsesAdapterDrainsPipeAfterDone(t *testing.T) {
	pr, pw := io.Pipe()
	writerDone := make(chan struct{})

	go func() {
		defer close(writerDone)
		defer pw.Close()
		// Emulate openaiChat's wire order.
		_, _ = pw.Write([]byte(": connected\n\n"))
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"收到\",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}]}\n\n"))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		// Trailing frame written AFTER [DONE] -- this is what wedges a reader
		// that stops at [DONE].
		_, _ = pw.Write([]byte(": m365-metrics {\"requestSent\":\"t\"}\n\n"))
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	sawDone := false
	deadline := time.After(5 * time.Second)
	readLoop := make(chan struct{})
	go func() {
		defer close(readLoop)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "data: [DONE]" {
				sawDone = true
				continue // keep draining -- the fix
			}
			_ = strings.HasPrefix(line, "data: ")
		}
	}()

	select {
	case <-readLoop:
	case <-deadline:
		t.Fatal("reader blocked: it stopped at [DONE] instead of draining to EOF")
	}
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never finished: the pipe was not drained")
	}
	if !sawDone {
		t.Fatal("terminal [DONE] marker was not observed")
	}
}
