package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Creates many folders that start with a non media file, which used to let the result
// channels close before the first job was added
func createTakeout(t *testing.T, folders int, filesPerFolder int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < folders; i++ {
		dir := filepath.Join(root, fmt.Sprintf("Album %03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0-metadata.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPerFolder; j++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("IMG_%04d.jpg", j)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// Runs Process and returns the last progress update
func runProcess(t *testing.T, ctx context.Context, source string, cancelAfter int, cancel context.CancelFunc) Progress {
	t.Helper()
	// Process writes its log file into the working directory
	t.Chdir(t.TempDir())

	progressCh := make(chan Progress)
	errCh := make(chan error, 1)
	go func() {
		errCh <- Process(ctx, source, filepath.Join(t.TempDir(), "out"), progressCh, ProcessOptions{})
	}()

	var last Progress
	timeout := time.After(30 * time.Second)
	for {
		select {
		case p, ok := <-progressCh:
			if !ok {
				<-errCh
				return last
			}
			last = p
			if cancel != nil && p.Processed >= cancelAfter {
				cancel()
			}
		case <-timeout:
			t.Fatal("Process did not finish in time")
		}
	}
}

func TestProcessAllFiles(t *testing.T) {
	source := createTakeout(t, 40, 10)
	last := runProcess(t, context.Background(), source, 0, nil)
	if last.Processed != 400 {
		t.Errorf("processed %d of 400 files", last.Processed)
	}
}

func TestProcessCancel(t *testing.T) {
	source := createTakeout(t, 40, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	last := runProcess(t, ctx, source, 10, cancel)
	if last.Processed >= 2000 {
		t.Errorf("all %d files were processed although processing was cancelled", last.Processed)
	}
}
