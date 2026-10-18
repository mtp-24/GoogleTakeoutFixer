/*
GoogleTakeoutFixer - A tool to easily clean and organize Google Photos Takeout exports
Copyright (C) 2026 feloex

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package fixer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A journal records every change the organize and in-place modes make to the user's files,
// one JSON object per line, so a run can be undone. It is written to the folder that is changed
type Journal struct {
	Path    string
	file    *os.File
	writer  *bufio.Writer
	mu      sync.Mutex
	Entries int
}

// One recorded change. Which fields are set depends on Op:
//
//	output:   Path (the output folder of the run, never removed by Undo)
//	move:     From, To. Recorded before the move happens, so a crash can never leave an unrecorded move
//	rmdir:    Path (an empty folder that was removed)
//	metadata: Path, Before (tag values before the change), ModTime, Backup (exiftool's _original copy, if kept)
type JournalEntry struct {
	Op      string            `json:"op"`
	From    string            `json:"from,omitempty"`
	To      string            `json:"to,omitempty"`
	Path    string            `json:"path,omitempty"`
	Before  map[string]string `json:"before,omitempty"`
	Added   []string          `json:"added,omitempty"`
	ModTime string            `json:"modTime,omitempty"`
	Backup  string            `json:"backup,omitempty"`
}

var (
	lastJournalPath string
	lastJournalMu   sync.Mutex
)

// LastJournalPath returns the journal of the most recent run that changed files, for the undo button
func LastJournalPath() string {
	lastJournalMu.Lock()
	defer lastJournalMu.Unlock()
	return lastJournalPath
}

// Creates a journal file like "GoogleTakeoutFixer-organize-2026-10-18_18-46-48.jsonl" in dir
func OpenJournal(dir string, kind string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("GoogleTakeoutFixer-%s-%s.jsonl", kind, time.Now().Format("2006-01-02_15-04-05")))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	lastJournalMu.Lock()
	lastJournalPath = path
	lastJournalMu.Unlock()
	return &Journal{Path: path, file: file, writer: bufio.NewWriter(file)}, nil
}

// Appends an entry and flushes it to disk, so a crash loses at most the change in progress
func (journal *Journal) Record(entry JournalEntry) error {
	if journal == nil {
		return nil
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := journal.writer.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := journal.writer.Flush(); err != nil {
		return err
	}
	journal.Entries++
	return journal.file.Sync()
}

func (journal *Journal) Close() error {
	if journal == nil {
		return nil
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if err := journal.writer.Flush(); err != nil {
		return err
	}
	return journal.file.Close()
}

// Reads all entries of a journal file
func ReadJournal(path string) ([]JournalEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []JournalEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var entry JournalEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("invalid journal line %d: %w", len(entries)+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// Moves a file without ever overwriting anything. Only works on the same volume, which keeps
// moves instant and means no file content is ever copied or rewritten
func MoveFile(from string, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists", to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		if isCrossDevice(err) {
			return fmt.Errorf("%s is on a different volume than %s. Files are only moved, not copied, so the output folder has to be on the same drive as the takeout", filepath.Dir(to), from)
		}
		return err
	}
	return nil
}

// Undo replays a journal backwards: moved files are moved back, removed folders are recreated
// and metadata changes are reverted. Progress messages are logged. Entries that cannot be undone,
// because a file was changed or moved since, are reported and skipped, so an undo can be re-run
func Undo(journalPath string) error {
	entries, err := ReadJournal(journalPath)
	if err != nil {
		return err
	}
	Log(LoggerInfo, "Undoing %d changes from %s", len(entries), journalPath)

	needExifTool := false
	for _, entry := range entries {
		if entry.Op == "metadata" {
			needExifTool = true
		}
	}
	if needExifTool {
		if err := InitializeExifTool(); err != nil {
			return err
		}
		defer CloseExifTool()
	}

	outputRoot := ""
	undone, skipped, failed := 0, 0, 0
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		var err error
		switch entry.Op {
		case "output":
			outputRoot = entry.Path
			continue
		case "move":
			_, toErr := os.Lstat(entry.To)
			_, fromErr := os.Lstat(entry.From)
			switch {
			case toErr == nil:
				err = MoveFile(entry.To, entry.From)
			case fromErr == nil:
				// The move never happened (the run stopped right after recording it) or was undone already
				skipped++
				continue
			default:
				err = fmt.Errorf("neither %s nor %s exists", entry.To, entry.From)
			}
		case "rmdir":
			err = os.MkdirAll(entry.Path, 0o755)
		case "metadata":
			err = undoMetadata(entry)
		default:
			err = fmt.Errorf("unknown journal operation %q", entry.Op)
		}
		if err != nil {
			failed++
			Log(LoggerError, "Could not undo %s %s%s: %v", entry.Op, entry.From, entry.Path, err)
		} else {
			undone++
		}
	}

	// Folders created for the output may now be empty. The output folder itself is left alone
	for _, entry := range entries {
		if entry.Op == "move" {
			removeEmptyParents(filepath.Dir(entry.To), outputRoot)
		}
	}

	Log(LoggerInfo, "Undo finished: %d changes undone, %d were already undone, %d could not be undone", undone, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d changes could not be undone, see the log", failed)
	}
	return nil
}

// Removes dir and its parents as long as they are empty. Stops at the first non-empty folder
// and never removes stopAt or anything above it
func removeEmptyParents(dir string, stopAt string) {
	for dir != stopAt && dir != filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
