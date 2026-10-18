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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Progress struct {
	Total     int
	Processed int
	Current   string
}

// TODO: Add more options
// TODO: Disable checkboxes when processing
type ProcessOptions struct {
	UseSymlinks         bool
	WriteMetadata       bool
	MonthSubfolders     bool
	IgnoreAlbums        bool
	Flatten             bool
	RestoreMOVExtension bool // See issue #2
	DryRun              bool
	SkipBin             bool // Do not export deleted photos from the Bin/Trash folder
	KeepBackups         bool // In-place mode: keep exiftool's "_original" copies so Undo restores the exact bytes
}

// Folder names Google uses for deleted photos
var binFolderNames = []string{"Bin", "Trash", "Papierkorb", "Corbeille", "Cestino", "Papelera", "Prullenbak", "Kosz", "Корзина"}

func isBinFolder(name string) bool {
	return slices.Contains(binFolderNames, name)
}

type FixerContext struct {
	Ctx         context.Context
	SourceRoot  string
	SourceRoots []string // Google Photos folders inside of SourceRoot, multiple when the input contains several takeouts
	OutputRoot  string
	Options     ProcessOptions
	ProgressCh  chan<- Progress
	Stats       ProcessStats

	// Destination paths written during this run and their source files, used to detect name collisions (issue #26)
	claimedPaths     map[string]string
	claimedPathsLock sync.Mutex
}

// Counters that are logged as a summary so skipped files do not go unnoticed (issue #26)
type ProcessStats struct {
	Found            atomic.Int64 // Media files queued for processing
	Written          atomic.Int64 // Media files written to the output
	Renamed          atomic.Int64 // Written under another name because the name was already used
	AlreadyExisted   atomic.Int64 // Skipped because the file already existed in the output before this run
	Duplicates       atomic.Int64 // Skipped because an identical file with the same name was already written
	Failed           atomic.Int64 // Media files that could not be processed
	MetadataFailed   atomic.Int64 // Written, but metadata could not be applied
	RenamedExtension atomic.Int64 // Written with the extension of their real file type
	NoSidecar        atomic.Int64 // Written without metadata because no sidecar was found
	Unsupported      atomic.Int64 // Files ignored because their type is not supported
}

// ClaimDestPath reserves a destination path for the source file. If another file of this run already
// uses the path, a free name like "IMG_0001(1).jpg" is returned instead. existed reports whether
// the file was already in the output before this run, in which case it should not be overwritten.
// duplicateOf is set if an identical file was already written under this name, for example
// because it is part of multiple takeout archives. The file should be skipped then.
func (fixerCtx *FixerContext) ClaimDestPath(destPath string, sourcePath string) (claimed string, existed bool, duplicateOf string) {
	fixerCtx.claimedPathsLock.Lock()
	defer fixerCtx.claimedPathsLock.Unlock()

	if fixerCtx.claimedPaths == nil {
		fixerCtx.claimedPaths = make(map[string]string)
	}

	for i := 0; ; i++ {
		candidate := filepath.Join(filepath.Dir(destPath), NameWithCounter(filepath.Base(destPath), i))

		// Lowercase keys because file systems like APFS and NTFS are case insensitive
		key := strings.ToLower(candidate)
		if claimedSource, ok := fixerCtx.claimedPaths[key]; ok {
			if SameFileContent(claimedSource, sourcePath) {
				return "", false, claimedSource
			}
			continue
		}

		if _, err := os.Lstat(candidate); err == nil {
			if i == 0 {
				return destPath, true, ""
			}
			continue
		}

		fixerCtx.claimedPaths[key] = sourcePath
		return candidate, false, ""
	}
}

// Log a summary of what happened to all files
func (stats *ProcessStats) LogSummary(dryRun bool) {
	written := "written"
	if dryRun {
		Log(LoggerInfo, "Dry run: nothing was changed, the summary shows what would happen")
		written = "would be written"
	}
	Log(LoggerInfo, "Summary: %d of %d media files %s (%d without metadata, %d renamed to avoid overwriting)",
		stats.Written.Load(), stats.Found.Load(), written, stats.NoSidecar.Load(), stats.Renamed.Load())

	if n := stats.Duplicates.Load(); n > 0 {
		Log(LoggerInfo, "Summary: %d files skipped because they are identical to another file with the same name", n)
	}
	if n := stats.AlreadyExisted.Load(); n > 0 {
		Log(LoggerWarn, "Summary: %d files skipped because they already existed in the output folder", n)
	}
	if n := stats.MetadataFailed.Load(); n > 0 {
		Log(LoggerWarn, "Summary: %d files with metadata that could not be applied", n)
	}
	if n := stats.RenamedExtension.Load(); n > 0 {
		Log(LoggerInfo, "Summary: %d files saved with the extension of their real file type", n)
	}
	if n := stats.Unsupported.Load(); n > 0 {
		Log(LoggerWarn, "Summary: %d files ignored because their file type is not supported", n)
	}
	if n := stats.Failed.Load(); n > 0 {
		Log(LoggerError, "Summary: %d files failed to process, search the log for ERROR to see which ones", n)
	}
}

// Process is the main fixer entry point.
// Process
// -> DiscoverDirs
// --> ProcessDirectory
// ---> ProcessFile
// TODO: Do something in case files already exists instead of overwriting them
func Process(
	ctx context.Context,
	sourcePath string,
	outputPath string,
	progressCh chan<- Progress,
	options ProcessOptions,
) error {
	err := InitializeFileLogger()
	if err != nil {
		if LogHandler != nil {
			LogHandler(LoggerWarn, fmt.Sprintf("Failed to initialize file logger: %v", err))
		}
	} else {
		defer func() {
			err := CloseFileLogger()
			if err != nil && LogHandler != nil {
				LogHandler(LoggerWarn, fmt.Sprintf("Failed to close file logger: %v", err))
			}
		}()
	}

	Log(LoggerInfo, "Starting processing with source: %s and output: %s", sourcePath, outputPath)
	if logPath := LogFilePath(); logPath != "" {
		Log(LoggerInfo, "Detailed log: %s", logPath)
	}

	// Log total processing time when processing is done
	startTime := time.Now()
	defer func() {
		Log(LoggerInfo, "Total processing time: %s", time.Since(startTime).Round(time.Second))
		ClearCache()
	}()

	defer close(progressCh)
	p := Progress{}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Restoring .MOV extensions only reads metadata, so exiftool is needed for it in a dry run as well
	if (options.WriteMetadata && !options.DryRun) || options.RestoreMOVExtension {
		if err := InitializeExifTool(); err != nil {
			Log(LoggerError, "Failed to initialize exiftool: %v", err)
			return err
		}
		defer CloseExifTool()
	}

	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		Log(LoggerError, "Error getting file info: %v", err)
		return err
	}

	fixerCtx := &FixerContext{
		Ctx:        ctx,
		SourceRoot: sourcePath,
		OutputRoot: outputPath,
		Options:    options,
		ProgressCh: progressCh,
	}

	// process all directories in the source directory, ignore files in the source directory itself
	// because all media files should be inside of sub-folders
	if fileInfo.IsDir() {
		roots, err := FindSourceRoots(sourcePath)
		if err != nil {
			Log(LoggerError, "Error finding Google Photos folders: %v", err)
			return err
		}
		fixerCtx.SourceRoots = roots
		if len(roots) > 1 || roots[0] != sourcePath {
			Log(LoggerInfo, "Found %d Google Photos folders: %s", len(roots), strings.Join(roots, ", "))
		}

		type sourceDir struct {
			path         string
			name         string
			isYearFolder bool
		}
		var yearDirs, albumDirs []sourceDir

		for _, root := range roots {
			dirs, err := DiscoverDirs(root)
			if err != nil {
				Log(LoggerError, "Error discovering directories in %s: %v", root, err)
				return err
			}

			for _, dir := range dirs {
				isYearFolder, err := IsYearFolder(dir.Name())
				if err != nil {
					Log(LoggerWarn, "Failed to determine if folder is a year folder for %s: %v", dir.Name(), err)
				}
				if (options.IgnoreAlbums || options.Flatten) && !isYearFolder {
					Log(LoggerInfo, "Skipping album folder: %s", dir.Name())
					continue
				}
				if options.SkipBin && isBinFolder(dir.Name()) {
					Log(LoggerInfo, "Skipping deleted photos in %s", filepath.Join(root, dir.Name()))
					continue
				}

				curDir := sourceDir{path: filepath.Join(root, dir.Name()), name: dir.Name(), isYearFolder: isYearFolder}
				if isYearFolder {
					yearDirs = append(yearDirs, curDir)
				} else {
					albumDirs = append(albumDirs, curDir)
				}
			}
		}

		// Only count the folders that are processed, so the progress is right when albums are skipped
		dirs := append(yearDirs, albumDirs...)
		for _, dir := range dirs {
			p.Total += CountMediaFiles(dir.path)
		}
		if p.Total == 0 {
			Log(LoggerError, "No media files found in the selected folders")
			return fmt.Errorf("no media files found in folder structure")
		}
		progressCh <- p

		// Process year folders of all takeouts before album folders, so album files can be
		// symlinked to their already existing counterparts in the year folders.
		// Folders with the same name in different takeouts end up in the same output folder
		for _, dir := range dirs {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			targetPath := filepath.Join(outputPath, dir.name)
			p = ProcessDirectory(fixerCtx, dir.path, targetPath, dir.isYearFolder, p)
		}
	} else {
		p.Total = 1
		progressCh <- p
		fixerCtx.Stats.Found.Add(1)
		err = ProcessFile(fixerCtx, sourcePath, "", false)
		if err != nil {
			fixerCtx.Stats.Failed.Add(1)
			Log(LoggerError, "Error processing file: %v", err)
		} else {
			p.Processed++
			p.Current = sourcePath
			progressCh <- p
		}
	}

	fixerCtx.Stats.LogSummary(options.DryRun)

	return nil
}

// Process a directory and fix all files within the directory. Ignores sub-directories.
func ProcessDirectory(
	fixerCtx *FixerContext,
	dirPath string,
	outputPath string,
	isYearFolder bool,
	p Progress,
) Progress {
	if fixerCtx.Ctx.Err() != nil {
		return p
	}

	files, err := os.ReadDir(dirPath)
	if err != nil {
		Log(LoggerError, "Error reading directory: %v", err)
		return p
	}

	workerCount := runtime.NumCPU() * 2 // x2 is faster for IO tasks, x more than that has no effect based on testing

	// Job pools
	// Only buffer enough jobs to keep the workers busy, so cancelling does not have to wait for a full queue
	jobs := make(chan string, workerCount)
	completed := make(chan string)
	// Channel to capture errors
	errors := make(chan error)

	sourceDirName := filepath.Base(dirPath)

	var wg sync.WaitGroup

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		go func() {
			for imagePath := range jobs {
				if fixerCtx.Ctx.Err() != nil {
					wg.Done()
					continue
				}
				err := ProcessFile(fixerCtx, imagePath, sourceDirName, isYearFolder)
				if err != nil {
					fixerCtx.Stats.Failed.Add(1)
					errors <- fmt.Errorf("error processing file %s: %w", imagePath, err)
				} else {
					completed <- imagePath
				}
				wg.Done()
			}
		}()
	}

	// Send jobs from a separate goroutine, because the jobs channel only has a small buffer
	// and the results below have to be read at the same time
	sendingDone := make(chan struct{})
	go func() {
		defer close(sendingDone)
		defer close(jobs)

		for _, file := range files {
			if fixerCtx.Ctx.Err() != nil {
				return
			}
			imagePath := filepath.Join(dirPath, file.Name())

			if file.IsDir() {
				Log(LoggerWarn, "Skipping nested folder %s, only files directly inside album and year folders are processed", imagePath)
				continue
			}

			// Check whether a file is a media file. Sidecars and hidden files like .DS_Store are expected
			if !IsMediaFile(imagePath) {
				if !IsNameExtension(".json", imagePath) && !strings.HasPrefix(file.Name(), ".") {
					fixerCtx.Stats.Unsupported.Add(1)
					Log(LoggerWarn, "Skipping unsupported file %s", imagePath)
				}
				continue
			}

			// Add work group before transmitting job
			wg.Add(1)
			select {
			case jobs <- imagePath:
				fixerCtx.Stats.Found.Add(1)
			case <-fixerCtx.Ctx.Done():
				wg.Done()
				return
			}
		}
	}()

	// Close completed and errors channels when all jobs are finished.
	// Wait for the sender first, otherwise wg.Wait could return before the first job was added
	go func() {
		<-sendingDone
		wg.Wait()
		close(completed)
		close(errors)
	}()

	// Update progress and handle errors.
	// Cancelling needs no extra case here: the sender stops and the workers skip remaining jobs
	for completed != nil || errors != nil {
		select {
		case ev, ok := <-completed:
			if !ok {
				completed = nil
			} else {
				p.Processed++
				p.Current = ev
				fixerCtx.ProgressCh <- p
			}
		case err, ok := <-errors:
			if !ok {
				errors = nil
			} else {
				Log(LoggerError, "%v", err)
			}
		}
	}

	return p
}

// ProcessFile processes a single file by finding its sidecar file and then fixing it using the sidecar's metadata
// TODO: This function is written unorganized and should be refactored
func ProcessFile(
	fixerCtx *FixerContext,
	sourcePath string,
	sourceDirName string,
	isYearFolder bool,
) error {
	fileName := filepath.Base(sourcePath)

	// See issue #2
	if fixerCtx.Options.RestoreMOVExtension && strings.EqualFold(filepath.Ext(fileName), ".mp4") {
		majorBrand, err := GetMajorBrand(sourcePath)
		if err == nil && strings.HasPrefix(majorBrand, "Apple QuickTime") {
			ext := filepath.Ext(fileName)
			newName := fileName[:len(fileName)-len(ext)] + ".mov"
			if ext == ".MP4" {
				newName = fileName[:len(fileName)-len(ext)] + ".MOV"
			}
			fileName = newName
		}
	}

	// Google exports some JPEGs as .png or .heic. Exiftool refuses to write those and photo apps
	// get confused, so the copy gets the extension of its real type
	if corrected := CorrectedFileName(sourcePath); corrected != filepath.Base(sourcePath) {
		Log(LoggerWarn, "%s is not a %s file but a %s, saving it as %s", sourcePath, strings.ToUpper(strings.TrimPrefix(filepath.Ext(sourcePath), ".")), strings.ToUpper(strings.TrimPrefix(filepath.Ext(corrected), ".")), corrected)
		fileName = corrected
		fixerCtx.Stats.RenamedExtension.Add(1)
	}

	sidecarPath, err := fixerCtx.FindSidecar(sourcePath)

	if err != nil {
		Log(LoggerError, "Error finding sidecar for file %s: %v", sourcePath, err)
		return err
	}

	// If no sidecar is found and its a video file, try to find a partner image and use it's sidecar
	if sidecarPath == "" && IsVideoFile(sourcePath) {
		partnerImage, err := FindImagePartner(sourcePath)
		if err == nil && partnerImage != "" {
			partnerSidecar, err := fixerCtx.FindSidecar(partnerImage)
			if err == nil && partnerSidecar != "" {
				sidecarPath = partnerSidecar
			}
		}
	}

	outputDir, err := ResolveOutputDir(fixerCtx, sourcePath, sidecarPath, sourceDirName, isYearFolder)
	if err != nil {
		return err
	}

	wantedPath := filepath.Join(outputDir, fileName)

	destPath, existed, duplicateOf := fixerCtx.ClaimDestPath(wantedPath, sourcePath)
	if duplicateOf != "" {
		fixerCtx.Stats.Duplicates.Add(1)
		Log(LoggerInfo, "Skipping %s, it is identical to %s", sourcePath, duplicateOf)
		return nil
	}
	if existed {
		fixerCtx.Stats.AlreadyExisted.Add(1)
		Log(LoggerWarn, "File %s already exists in the output, skipping %s", destPath, sourcePath)
		return nil
	}
	if destPath != wantedPath {
		fixerCtx.Stats.Renamed.Add(1)
		Log(LoggerWarn, "File name %s is already used by another file, saving %s as %s", wantedPath, sourcePath, filepath.Base(destPath))
	}

	// Metadata sidecar file not found, copy the file without metadata
	if sidecarPath == "" {
		Log(LoggerWarn, "No sidecar file found for %s — copying without metadata", sourcePath)
		if err := CreateFixedFile(fixerCtx, sourcePath, "", destPath, isYearFolder); err != nil {
			Log(LoggerError, "Error creating file without sidecar for %s: %v", sourcePath, err)
			return err
		}
		fixerCtx.Stats.NoSidecar.Add(1)
		fixerCtx.Stats.Written.Add(1)
		return nil
	}

	err = CreateFixedFile(fixerCtx, sourcePath, sidecarPath, destPath, isYearFolder)
	if err != nil {
		Log(LoggerError, "Error creating fixed file for %s: %v", sourcePath, err)
		return err
	}
	fixerCtx.Stats.Written.Add(1)

	return nil
}

// Finds the sidecar of a media file. Google sometimes puts a media file and its sidecar
// into different takeout archives, so the same folder of the other takeouts is searched as well
func (fixerCtx *FixerContext) FindSidecar(mediaPath string) (string, error) {
	mediaDir := filepath.Dir(mediaPath)
	dirs := []string{mediaDir}
	for _, root := range fixerCtx.SourceRoots {
		otherDir := filepath.Join(root, filepath.Base(mediaDir))
		if otherDir != mediaDir {
			dirs = append(dirs, otherDir)
		}
	}

	sidecarPath, err := FindSidecarIn(dirs, mediaPath)
	if err != nil {
		return "", err
	}
	if sidecarPath != "" && filepath.Dir(sidecarPath) != mediaDir {
		Log(LoggerInfo, "Found sidecar for %s in another takeout: %s", mediaPath, sidecarPath)
	}
	return sidecarPath, nil
}

// Checks whether a file exists in the output. In a dry run nothing is written, so files that
// would have been written earlier in this run count as existing as well
func (fixerCtx *FixerContext) outputFileExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	if !fixerCtx.Options.DryRun {
		return false
	}

	fixerCtx.claimedPathsLock.Lock()
	defer fixerCtx.claimedPathsLock.Unlock()
	_, claimed := fixerCtx.claimedPaths[strings.ToLower(path)]
	return claimed
}

// Attempts to find the copy of a file inside of any year folder in the output,
// so album files can be symlinked to it. Returns an empty string if there is none
func FindSymlinkTarget(fixerCtx *FixerContext, filePath string, fileMetadataPath string, fileName string) string {
	monthFolder := ""
	if fixerCtx.Options.MonthSubfolders {
		month, err := DetectFileMonth(filePath, fileMetadataPath)
		if err == nil {
			monthFolder = strconv.Itoa(month)
		}
	}

	// In a dry run the output year folders do not exist, but they are named like the source year folders
	var entries []os.DirEntry
	if fixerCtx.Options.DryRun {
		for _, root := range fixerCtx.SourceRoots {
			rootEntries, _ := ReadDirCached(root)
			entries = append(entries, rootEntries...)
		}
	} else {
		entries, _ = ReadDirCached(fixerCtx.OutputRoot)
	}

	var refused []string
	for _, curEntry := range entries {
		if !curEntry.IsDir() {
			continue
		}

		isYear, _ := IsYearFolder(curEntry.Name())
		if !isYear {
			continue
		}

		targetPaths := []string{}
		if monthFolder != "" {
			targetPaths = append(targetPaths, filepath.Join(fixerCtx.OutputRoot, curEntry.Name(), monthFolder, fileName))
		}
		targetPaths = append(targetPaths, filepath.Join(fixerCtx.OutputRoot, curEntry.Name(), fileName))

		for _, target := range targetPaths {
			if !fixerCtx.outputFileExists(target) {
				continue
			}
			// Album files are often named like unrelated files in the year folders (e.g. "2024-01-04(10).jpg"),
			// so only link to a file that really has the same content
			if fixerCtx.sameOutputContent(target, filePath) {
				return target
			}
			refused = append(refused, target)
		}
	}

	if len(refused) > 0 {
		Log(LoggerInfo, "Not linking %s, the same name in the year folders belongs to a different photo: %s", filePath, strings.Join(refused, ", "))
	}
	return ""
}

// Compares the source of a file in the output with another source file. In a dry run the output
// file does not exist yet, so the source file it would be written from is compared instead
func (fixerCtx *FixerContext) sameOutputContent(outputPath string, sourcePath string) bool {
	if fixerCtx.Options.DryRun {
		fixerCtx.claimedPathsLock.Lock()
		claimedSource, ok := fixerCtx.claimedPaths[strings.ToLower(outputPath)]
		fixerCtx.claimedPathsLock.Unlock()
		if ok {
			return SameFileContent(claimedSource, sourcePath)
		}
	}
	return SameFileContent(outputPath, sourcePath)
}

func CreateFixedFile(
	fixerCtx *FixerContext,
	filePath string,
	fileMetadataPath string,
	destPath string,
	isYearFolder bool,
) error {
	if !fixerCtx.Options.DryRun {
		// Ensure output directory exists (create if not)
		destDir := filepath.Dir(destPath)
		if _, err := os.Stat(destDir); os.IsNotExist(err) {
			if err := os.MkdirAll(destDir, 0755); err != nil {
				return err
			}

			// Invalidate OutputRoot cache so newly created year folders are visible for symlinks
			ClearCacheDir(fixerCtx.OutputRoot)
		}
	}

	if fixerCtx.Options.UseSymlinks && !isYearFolder {
		if target := FindSymlinkTarget(fixerCtx, filePath, fileMetadataPath, filepath.Base(destPath)); target != "" {
			if fixerCtx.Options.DryRun {
				Log(LoggerInfo, "[dry-run] Would symlink %s -> %s", destPath, target)
				return nil
			}

			if err := os.Symlink(target, destPath); err == nil {
				// Symlink successful
				return nil
			} else if !os.IsExist(err) {
				return fmt.Errorf("Failed to create symlink: %w", err)
			}
			// Symlink failed, continue with normal copy
		}
	}

	if fixerCtx.Options.DryRun {
		Log(LoggerInfo, "[dry-run] Would copy %s -> %s", filePath, destPath)
		if fixerCtx.Options.WriteMetadata && fileMetadataPath != "" {
			// Read the sidecar so broken sidecars show up in the dry run as well
			if err := CheckJsonMetadata(fileMetadataPath); err != nil {
				fixerCtx.Stats.MetadataFailed.Add(1)
				Log(LoggerError, "Failed to read metadata from %s: %v", fileMetadataPath, err)
			} else {
				Log(LoggerInfo, "[dry-run] Would apply metadata from %s", fileMetadataPath)
			}
		}
		return nil
	}

	if err := DuplicateFile(filePath, destPath); err != nil {
		return err
	}

	if fixerCtx.Options.WriteMetadata && fileMetadataPath != "" {
		metadata, err := ReadJsonMetadata(fileMetadataPath)
		if err != nil {
			fixerCtx.Stats.MetadataFailed.Add(1)
			Log(LoggerError, "Failed to read metadata from %s: %v", fileMetadataPath, err)
		} else {
			// Only apply metadata if reading was successful
			err = ApplyMetadata(destPath, filepath.Base(filePath), metadata)
			if err != nil {
				fixerCtx.Stats.MetadataFailed.Add(1)
				Log(LoggerError, "Failed to apply metadata to %s: %v", destPath, err)
				explainMinorError(err)

				// At least give the file the right dates, so it does not show up as taken today
				if takenTime, timeErr := TakenTime(metadata); timeErr == nil {
					if dateErr := SetFileDates(destPath, takenTime); dateErr != nil {
						Log(LoggerError, "Failed to set file dates of %s: %v", destPath, dateErr)
					}
				}
			}
		}
	} else if fixerCtx.Options.WriteMetadata && fileMetadataPath == "" {
		// Without a sidecar the copy would get the current date as file date, so use the date
		// from the file's own metadata or from its name instead
		fileName := filepath.Base(destPath)
		date, found := ReadExifDate(destPath)
		source := "its metadata"
		if !found {
			date, found = ParseDateFromFileName(filepath.Base(filePath))
			source = "its file name"
		}

		if !found {
			Log(LoggerInfo, "WriteMetadata enabled but no sidecar or date found for %s — skipping metadata write", fileName)
		} else if err := SetFileDates(destPath, date); err != nil {
			fixerCtx.Stats.MetadataFailed.Add(1)
			Log(LoggerError, "Failed to set file dates of %s: %v", destPath, err)
		} else {
			Log(LoggerInfo, "No sidecar for %s, set file dates to %s from %s", fileName, date.Format("2006-01-02 15:04:05"), source)
		}
	}

	return nil
}
