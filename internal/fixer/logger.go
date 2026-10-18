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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type LogLevel string

const (
	LoggerInfo  LogLevel = "INFO"
	LoggerWarn  LogLevel = "WARN"
	LoggerError LogLevel = "ERROR"
)

// LogHandler allows the GUI or CLI to intercept logs
var LogHandler func(level LogLevel, message string)

var (
	logFileMu sync.Mutex
	logFile   *os.File
)

// LogDirectory returns the folder the detailed log files are written to. The app used to write
// into a "logs" folder relative to the working directory, which ends up in random places when
// the app is started by double-clicking it
func LogDirectory() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Logs", "GoogleTakeoutFixer")
		}
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "GoogleTakeoutFixer", "logs")
	}
	return "logs"
}

// LogFilePath returns the path of the current log file, or "" if none is open
func LogFilePath() string {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	if logFile == nil {
		return ""
	}
	return logFile.Name()
}

func InitializeFileLogger() error {
	logFileMu.Lock()
	defer logFileMu.Unlock()

	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}

	logDir := LogDirectory()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}

	fileName := fmt.Sprintf("%s.txt", time.Now().Format("2006-01-02_15-04-05"))
	filePath := filepath.Join(logDir, fileName)

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	logFile = file
	return nil
}

func CloseFileLogger() error {
	logFileMu.Lock()
	defer logFileMu.Unlock()

	if logFile == nil {
		return nil
	}

	err := logFile.Close()
	logFile = nil
	return err
}

// Send a log message to the handler
func Log(level LogLevel, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format(time.RFC3339)
	logLine := fmt.Sprintf("[%s] [%s] %s\n", timestamp, level, msg)

	logFileMu.Lock()
	if logFile != nil {
		_, _ = logFile.WriteString(logLine)
	}
	logFileMu.Unlock()

	if LogHandler != nil {
		LogHandler(level, msg)
	}
}
