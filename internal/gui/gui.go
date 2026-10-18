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

package gui

import (
	"context"
	"fmt"
	"image/color"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/feloex/GoogleTakeoutFixer/internal/fixer"
	version "github.com/feloex/GoogleTakeoutFixer/internal/version"
	"github.com/ncruces/zenity"
)

func Main() {
	var inputPath, outputPath string

	// Create app / window
	a := app.New()
	a.SetIcon(resourceGoogleTakeoutFixerPng)
	w := a.NewWindow("GoogleTakeoutFixer " + version.Tag)
	w.Resize(fyne.NewSize(550, 400))

	var useSymlinks bool = false
	var writeMetadata bool = true
	var flatten bool = false
	var ignoreAlbums bool = false
	var monthSubfolders bool = false
	var restoreMOVExtension bool = false
	var dryRun bool = false
	var skipBin bool = false
	var organize bool = false
	var inPlace bool = false
	var keepBackups bool = false

	progressLabel := widget.NewLabel("Ready to start")
	progressLabel.Truncation = fyne.TextTruncateEllipsis
	progressBar := widget.NewProgressBar()
	var cancelFn context.CancelFunc
	var cancelButton *widget.Button

	// Update checker
	go func() {
		latestVersion, err := version.CheckForUpdates()
		if err != nil {
			return
		}
		url, err := url.Parse(latestVersion.DownloadURL)
		if err != nil {
			return
		}
		fyne.Do(func() {

			updateMsg := fmt.Sprintf("Download new version: %s", latestVersion.Version)
			updateLink := widget.NewHyperlink(updateMsg, url)
			updateLink.Alignment = fyne.TextAlignCenter
			updateLink.TextStyle = fyne.TextStyle{Bold: true}

			updateLink.Refresh()

			// Keep the main content in the center so it resizes properly
			w.SetContent(container.NewBorder(
				updateLink,
				nil,
				nil,
				nil,
				w.Content(),
			))
		})
	}()

	// Button for opening file dialog for choosing google takeout path and output path
	var inputButton *widget.Button
	inputButton = widget.NewButtonWithIcon("Select Google Takeout Folder", theme.FolderOpenIcon(), func() {
		dir, err := zenity.SelectFile(zenity.Title("Select Google Takeout Folder"), zenity.Directory())
		if err == nil {
			inputPath = dir
			inputButton.SetText("Input: " + filepath.Base(inputPath))
			fmt.Println("Input folder:", inputPath)
		}
	})

	var outputButton *widget.Button
	outputButton = widget.NewButtonWithIcon("Select Output Folder", theme.FolderOpenIcon(), func() {
		dir, err := zenity.SelectFile(zenity.Title("Select Output Folder"), zenity.Directory())
		if err == nil {
			outputPath = dir
			outputButton.SetText("Output: " + filepath.Base(outputPath))
			fmt.Println("Output folder:", outputPath)
		}
	})

	// Checkboxes for options. Default value is false
	useLinksCheckbox := widget.NewCheck("Use symlinks for albums", func(value bool) {
		useSymlinks = value
		fmt.Println("use symlinks", useSymlinks)
	})

	writeMetadataCheckbox := widget.NewCheck("Write metadata", func(value bool) {
		writeMetadata = value
		fmt.Println("write metadata", writeMetadata)
	})
	writeMetadataCheckbox.SetChecked(true)

	ignoreAlbumsCheckbox := widget.NewCheck("Ignore album folders", func(value bool) {
		ignoreAlbums = value
		fmt.Println("ignore albums", ignoreAlbums)
	})

	monthSubfoldersCheckbox := widget.NewCheck("Create month subfolders", func(value bool) {
		monthSubfolders = value
		fmt.Println("month subfolders", monthSubfolders)
	})

	flattenCheckbox := widget.NewCheck("Flatten album structure", func(value bool) {
		flatten = value
		fmt.Println("flatten", flatten)
	})

	restoreMOVExtensionCheckbox := widget.NewCheck("Restore .MOV file extension", func(value bool) {
		restoreMOVExtension = value
		fmt.Println("restore MOV extension", restoreMOVExtension)
	})

	dryRunCheckbox := widget.NewCheck("Dry run (no changes)", func(value bool) {
		dryRun = value
		fmt.Println("dry run", dryRun)
	})

	skipBinCheckbox := widget.NewCheck("Skip Bin (deleted photos)", func(value bool) {
		skipBin = value
		fmt.Println("skip bin", skipBin)
	})

	// Modes that change the input instead of copying it. Both write a journal that "Undo last run" replays
	organizeCheckbox := widget.NewCheck("Consolidate takeouts by moving files (no copies)", func(value bool) {
		organize = value
		fmt.Println("organize", organize)
	})

	inPlaceCheckbox := widget.NewCheck("Fix files in place (no copies)", func(value bool) {
		inPlace = value
		fmt.Println("in place", inPlace)
	})

	keepBackupsCheckbox := widget.NewCheck("Keep backup copies for a byte-exact undo (needs disk space)", func(value bool) {
		keepBackups = value
		fmt.Println("keep backups", keepBackups)
	})
	keepBackupsCheckbox.Disable()

	var undoButton *widget.Button

	// Fix conflicting options
	updateCheckboxStates := func() {
		setEnabled := func(cb *widget.Check, enabled bool) {
			if enabled {
				cb.Enable()
			} else {
				cb.Disable()
			}
		}
		setEnabled(useLinksCheckbox, !ignoreAlbums && !flatten && !organize && !inPlace)
		setEnabled(ignoreAlbumsCheckbox, !useSymlinks && !flatten)
		setEnabled(flattenCheckbox, !useSymlinks && !ignoreAlbums && !monthSubfolders && !inPlace)
		setEnabled(monthSubfoldersCheckbox, !flatten && !inPlace)
		setEnabled(organizeCheckbox, !inPlace && !useSymlinks)
		setEnabled(inPlaceCheckbox, !organize && !useSymlinks && !flatten && !monthSubfolders)
		setEnabled(keepBackupsCheckbox, inPlace)
		setEnabled(writeMetadataCheckbox, !organize)
		// Fixing in place needs no output folder
		if inPlace {
			outputButton.Disable()
		} else {
			outputButton.Enable()
		}
	}

	for _, cb := range []*widget.Check{useLinksCheckbox, ignoreAlbumsCheckbox, flattenCheckbox, monthSubfoldersCheckbox, organizeCheckbox, inPlaceCheckbox} {
		cb := cb
		prev := cb.OnChanged
		cb.OnChanged = func(v bool) {
			prev(v)
			updateCheckboxStates()
		}
	}

	// Button to start processing
	var startButton *widget.Button
	startButton = widget.NewButtonWithIcon("Start Processing", theme.MediaPlayIcon(), func() {
		// one of the folders has not been selected
		if inputPath == "" || (outputPath == "" && !inPlace) {
			fixer.Log(fixer.LoggerInfo, "Select both input and output folders")
			return
		}

		// Disable buttons while processing
		inputButton.Disable()
		outputButton.Disable()
		startButton.Disable()

		useLinksCheckbox.Disable()
		writeMetadataCheckbox.Disable()
		ignoreAlbumsCheckbox.Disable()
		monthSubfoldersCheckbox.Disable()
		flattenCheckbox.Disable()
		restoreMOVExtensionCheckbox.Disable()
		dryRunCheckbox.Disable()
		skipBinCheckbox.Disable()
		organizeCheckbox.Disable()
		inPlaceCheckbox.Disable()
		keepBackupsCheckbox.Disable()
		undoButton.Disable()

		fixer.Log(fixer.LoggerInfo, "Processing...")
		progressBar.SetValue(0)

		ctx, cancel := context.WithCancel(context.Background())
		cancelFn = cancel
		cancelButton.Enable()

		progressCh := make(chan fixer.Progress)

		opts := fixer.ProcessOptions{
			UseSymlinks:         useSymlinks,
			WriteMetadata:       writeMetadata,
			Flatten:             flatten,
			IgnoreAlbums:        ignoreAlbums,
			MonthSubfolders:     monthSubfolders,
			RestoreMOVExtension: restoreMOVExtension,
			DryRun:              dryRun,
			SkipBin:             skipBin,
			KeepBackups:         keepBackups,
		}
		go func() {
			var err error
			switch {
			case organize:
				err = fixer.Organize(ctx, inputPath, outputPath, progressCh, opts)
			case inPlace:
				err = fixer.ProcessInPlace(ctx, inputPath, progressCh, opts)
			default:
				err = fixer.Process(ctx, inputPath, outputPath, progressCh, opts)
			}
			if err != nil {
				if ctx.Err() == nil {
					fyne.Do(func() {
						fixer.Log(fixer.LoggerError, "%s", "Error: "+err.Error())
					})
				}
			}
		}()

		// Update progress

		go func() {
			var lastUpdate time.Time
			var lastP fixer.Progress

			for p := range progressCh {
				lastP = p
				if time.Since(lastUpdate) >= 100*time.Millisecond {
					lastUpdate = time.Now()

					percentage := 0.0
					if p.Total > 0 {
						percentage = (float64(p.Processed) / float64(p.Total)) * 100.0
					}

					text := fmt.Sprintf("[%.2f%%] %d/%d - %s", percentage, p.Processed, p.Total, filepath.Base(p.Current))
					processed, total := float64(p.Processed), float64(p.Total)

					fyne.Do(func() {
						progressBar.Max = total
						progressBar.SetValue(processed)
						progressLabel.SetText(text)
					})
				}
			}

			// Processing complete
			fyne.Do(func() {
				if lastP.Total > 0 {
					progressBar.Max = float64(lastP.Total)
					progressBar.SetValue(float64(lastP.Total))
				}

				if ctx.Err() != nil {
					fixer.Log(fixer.LoggerInfo, "Cancelled")
					progressLabel.SetText("Cancelled")
				} else {
					fixer.Log(fixer.LoggerInfo, "Detailed logs are saved in %s", fixer.LogDirectory())
					fixer.Log(fixer.LoggerInfo, "Done")

					finishedText := fmt.Sprintf("Finished processing %d files", lastP.Processed)
					if opts.DryRun {
						finishedText = fmt.Sprintf("Dry run finished, %d files checked and nothing was changed", lastP.Processed)
					}
					progressLabel.SetText(finishedText)
					fixer.Log(fixer.LoggerInfo, "%s", finishedText)
				}
				cancelButton.Disable()
				cancelFn = nil
				inputButton.Enable()
				outputButton.Enable()
				startButton.Enable()

				// Manually re-enable checkboxes not affected by updateCheckboxStates
				restoreMOVExtensionCheckbox.Enable()
				dryRunCheckbox.Enable()
				skipBinCheckbox.Enable()
				if fixer.LastJournalPath() != "" {
					undoButton.Enable()
				}
				// Re-enable checboxes based on current states
				updateCheckboxStates()
			})
		}()
	})

	cancelButton = widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
		if cancelFn == nil {
			return
		}
		fixer.Log(fixer.LoggerInfo, "Cancelling...")
		cancelButton.Disable()
		cancelFn()
	})
	cancelButton.Disable()

	// Undoes the last run that moved files or changed metadata, using its journal
	undoButton = widget.NewButtonWithIcon("Undo last run", theme.ContentUndoIcon(), func() {
		journalPath := fixer.LastJournalPath()
		if journalPath == "" {
			return
		}
		dialog.ShowConfirm("Undo last run", "Move all files back and restore the metadata recorded in\n"+journalPath+"?", func(confirmed bool) {
			if !confirmed {
				return
			}
			undoButton.Disable()
			startButton.Disable()
			go func() {
				err := fixer.Undo(journalPath)
				fyne.Do(func() {
					if err != nil {
						fixer.Log(fixer.LoggerError, "Undo failed: %v", err)
						undoButton.Enable()
					} else {
						progressLabel.SetText("Undo finished")
					}
					startButton.Enable()
				})
			}()
		}, w)
	})
	undoButton.Disable()

	logEntry := widget.NewMultiLineEntry()
	const maxVisibleLogLines = 200
	visibleLogLines := make([]string, 0, maxVisibleLogLines)

	// Prevent user from editing the log while keeping text selectable
	// This is not optimal but fyne does not provide a better way to do this
	var logUpdating bool
	logEntry.OnChanged = func(_ string) {
		if logUpdating {
			return
		}
		logUpdating = true
		logEntry.SetText(strings.Join(visibleLogLines, "\n") + "\n")
		logUpdating = false
	}

	logCh := make(chan string, 1000)
	fixer.LogHandler = func(level fixer.LogLevel, message string) {
		logCh <- fmt.Sprintf("[%s] %s", level, message)
	}

	// Throttle log updates to the UI using a channel
	go func() {
		for logMsg := range logCh {
			newLogs := []string{logMsg}

			// Group remaining logs
			for len(logCh) > 0 {
				newLogs = append(newLogs, <-logCh)
			}

			fyne.Do(func() {
				visibleLogLines = append(visibleLogLines, newLogs...)
				if len(visibleLogLines) > maxVisibleLogLines {
					visibleLogLines = visibleLogLines[len(visibleLogLines)-maxVisibleLogLines:]
				}

				logUpdating = true
				logEntry.SetText(strings.Join(visibleLogLines, "\n") + "\n")
				logUpdating = false

				logEntry.CursorRow = len(visibleLogLines)
				logEntry.CursorColumn = 0
				logEntry.Refresh()
			})

			// Wait to collect more logs
			time.Sleep(100 * time.Millisecond)
		}
	}()

	fixer.Log(fixer.LoggerInfo, "Logs will appear here...")

	folderButtons := container.NewGridWithColumns(
		2,
		inputButton,
		outputButton,
	)

	CheckBoxRow := container.NewGridWithColumns(
		2,
		useLinksCheckbox,
		writeMetadataCheckbox,
		ignoreAlbumsCheckbox,
		monthSubfoldersCheckbox,
		flattenCheckbox,
		restoreMOVExtensionCheckbox,
		skipBinCheckbox,
		dryRunCheckbox,
		organizeCheckbox,
		inPlaceCheckbox,
		keepBackupsCheckbox,
	)

	StartCancelRow := container.NewGridWithColumns(3, startButton, cancelButton, undoButton)

	separator := canvas.NewRectangle(color.RGBA{R: 60, G: 60, B: 60, A: 100})
	separator.SetMinSize(fyne.NewSize(1, 1))

	FolderSeperator := container.NewPadded(separator)
	OptionsSeparator := container.NewPadded(separator)

	topContent := container.NewVBox(
		folderButtons,
		FolderSeperator,
		CheckBoxRow,
		OptionsSeparator,
		StartCancelRow,
		progressBar,
		progressLabel,
	)

	mainContent := container.NewBorder(
		topContent,
		nil,
		nil,
		nil,
		//logEntry, // expand
		logEntry, // expand
	)

	paddingMainContainer := container.NewPadded(mainContent)

	w.SetContent(paddingMainContainer)

	w.ShowAndRun()
}
