package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type promptReader struct {
	io.Reader
	started chan struct{}
}

func (r promptReader) Read(p []byte) (int, error) { close(r.started); return r.Reader.Read(p) }

func TestPromptCancellationWhileWaitingForInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	p := prompter{ctx: ctx, in: bufio.NewReader(promptReader{Reader: reader, started: started})}
	done := make(chan error, 1)
	go func() { _, err := p.ask("Role", "coordinator"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start reading")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled prompt is still waiting for input")
	}
}
func TestPromptDoesNotConsumeInputAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := bufio.NewReader(strings.NewReader("worker\n"))
	p := prompter{ctx: ctx, in: input}
	if _, err := p.ask("Role", "coordinator"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if line, err := input.ReadString('\n'); err != nil || line != "worker\n" {
		t.Fatal("canceled prompt consumed input", line, err)
	}
}
