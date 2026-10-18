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
	"sort"
	"strings"
	"time"
)

// Folder names inside the output for files that need a closer look
const (
	duplicatesFolder = "_duplicates"
	unmatchedFolder  = "_unmatched sidecars"
	unsortedFolder   = "_unsorted"
)

// A folder of a takeout that is processed, e.g. "Takeout 3/Google Photos/Photos from 2020"
type sourceDir struct {
	path         string
	name         string
	isYearFolder bool
}

// Lists the year and album folders of all Google Photos roots, year folders first,
// honoring the options that skip folders
func collectSourceDirs(roots []string, options ProcessOptions) ([]sourceDir, error) {
	var yearDirs, albumDirs []sourceDir
	for _, root := range roots {
		dirs, err := DiscoverDirs(root)
		if err != nil {
			return nil, fmt.Errorf("error discovering directories in %s: %w", root, err)
		}
		for _, dir := range dirs {
			isYearFolder, _ := IsYearFolder(dir.Name())
			if (options.IgnoreAlbums || options.Flatten) && !isYearFolder {
				Log(LoggerInfo, "Skipping album folder: %s", filepath.Join(root, dir.Name()))
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
	return append(yearDirs, albumDirs...), nil
}

// Opens the log file and logs the start of a run. The returned function closes the log
func startRun(mode string, sourcePath string, outputPath string) func() {
	if err := InitializeFileLogger(); err != nil && LogHandler != nil {
		LogHandler(LoggerWarn, fmt.Sprintf("Failed to initialize file logger: %v", err))
	}
	Log(LoggerInfo, "Starting %s with source: %s and output: %s", mode, sourcePath, outputPath)
	if logPath := LogFilePath(); logPath != "" {
		Log(LoggerInfo, "Detailed log: %s", logPath)
	}
	startTime := time.Now()
	return func() {
		Log(LoggerInfo, "Total processing time: %s", time.Since(startTime).Round(time.Second))
		ClearCache()
		if err := CloseFileLogger(); err != nil && LogHandler != nil {
			LogHandler(LoggerWarn, fmt.Sprintf("Failed to close file logger: %v", err))
		}
	}
}

type organizeStats struct {
	moved, duplicates, renamed, extRenamed, sidecars, unmatched, other, dirsRemoved int
}

type organizer struct {
	ctx      *FixerContext
	journal  *Journal
	dryRun   bool
	planned  map[string]string // Lowercased destination -> source of moves planned in a dry run
	moved    map[string]bool   // Source paths that were moved
	sidecars map[string]bool   // Sidecar source paths that were moved
	borrowed map[string]string // Sidecars used by a file that does not own them -> that file's output folder
	inOutput map[string]bool   // Output folder + media base name of sidecars already moved there
	stats    organizeStats
}

// Whether a path exists in the output, counting moves planned in a dry run. Returns the file
// to compare content with: the file itself, or in a dry run the source it would be moved from
func (org *organizer) exists(path string) (string, bool) {
	if _, err := os.Lstat(path); err == nil {
		return path, true
	}
	if src, ok := org.planned[strings.ToLower(path)]; ok {
		return src, true
	}
	return "", false
}

// Moves a file into the output, or logs the move in a dry run. The journal entry is written
// first: if the program dies in between, Undo sees a move whose source still exists and skips it
func (org *organizer) move(from string, to string) error {
	if org.dryRun {
		Log(LoggerInfo, "[dry-run] Would move %s -> %s", from, to)
	} else {
		if err := org.journal.Record(JournalEntry{Op: "move", From: from, To: to}); err != nil {
			return fmt.Errorf("could not write the journal: %w", err)
		}
		if err := MoveFile(from, to); err != nil {
			return err
		}
	}
	org.planned[strings.ToLower(to)] = from
	org.moved[from] = true
	// Directory listings are cached, keep them in line with the moves
	ClearCacheDir(filepath.Dir(from))
	ClearCacheDir(filepath.Dir(to))
	return nil
}

// Returns a path in dir for the file name that does not exist yet, adding "(n)" if needed
func (org *organizer) freePath(dir string, fileName string) string {
	ext := filepath.Ext(fileName)
	stem := strings.TrimSuffix(fileName, ext)
	for n := 0; ; n++ {
		candidate := filepath.Join(dir, fileName)
		if n > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s(%d)%s", stem, n, ext))
		}
		if _, exists := org.exists(candidate); !exists {
			return candidate
		}
	}
}

// Moves a file into one of the special folders, keeping the takeout folder name
func (org *organizer) moveAside(from string, special string, folderName string) error {
	dest := org.freePath(filepath.Join(org.ctx.OutputRoot, special, folderName), filepath.Base(from))
	return org.move(from, dest)
}

// Whether a media file is the file its sidecar was exported for, as opposed to a file that
// only borrows the sidecar (edited copies, live photo videos, Pixel motion photo videos)
func ownsSidecar(mediaPath string, sidecarPath string) bool {
	sidecarName, index, ok := parseSidecarName(filepath.Base(sidecarPath))
	if !ok {
		return false
	}
	parts := splitName(filepath.Base(mediaPath))
	// The name Google exported the sidecar for keeps any edited suffix: "IMG_0001-edited(1).jpg"
	// owns "IMG_0001-edited.jpg(1).json" but not "IMG_0001.jpg(1).json", which belongs to IMG_0001(1).jpg
	exported := parts.stemNoCounter + parts.ext
	switch {
	case sidecarName == parts.lower:
		return true
	case index != parts.trailing:
		return false
	case sidecarName == exported, sidecarName == parts.stemNoCounter:
		return true
	case len(filepath.Base(sidecarPath)) >= truncatedSidecarNameLength && strings.HasPrefix(exported, sidecarName):
		return true
	}
	return false
}

// A media file of a group and where it goes
type plannedMove struct {
	src       string
	sidecar   string
	destDir   string
	name      string // Final file name without a counter
	duplicate string // The identical file already in the output, if any
}

// Name with the group's counter, e.g. "IMG_0001(1).HEIC"
func (pm plannedMove) nameWithCounter(n int) string {
	if n == 0 {
		return pm.name
	}
	ext := filepath.Ext(pm.name)
	return fmt.Sprintf("%s(%d)%s", strings.TrimSuffix(pm.name, ext), n, ext)
}

// Organize moves the media files and sidecars of one or more takeout archives into a single
// folder structure in outputPath, without copying or changing any file content:
//
//   - the same folder of all takeouts (e.g. "Photos from 2020" or an album) becomes one folder
//   - a sidecar that Google put into a different archive than its photo ends up next to the photo
//   - files that are byte-identical to a file already in place go to "_duplicates"
//   - different files with the same name get a counter, together with their edited copies,
//     live photo videos and sidecars, so they keep belonging together
//   - files whose extension does not match their content are renamed to the real type
//   - sidecars without a media file go to "_unmatched sidecars", unknown files to "_unsorted"
//   - folders that are left empty are removed
//
// Every change is written to a journal in the output folder, which Undo can replay backwards.
// Output and takeouts have to be on the same volume, because files are only ever renamed
func Organize(
	ctx context.Context,
	sourcePath string,
	outputPath string,
	progressCh chan<- Progress,
	options ProcessOptions,
) error {
	finish := startRun("organizing", sourcePath, outputPath)
	defer finish()
	defer close(progressCh)

	if options.UseSymlinks {
		return fmt.Errorf("symlinks cannot be used when organizing in place")
	}

	if err := checkOrganizePaths(sourcePath, outputPath); err != nil {
		Log(LoggerError, "%v", err)
		return err
	}

	roots, err := FindSourceRoots(sourcePath)
	if err != nil {
		Log(LoggerError, "Error finding Google Photos folders: %v", err)
		return err
	}
	Log(LoggerInfo, "Found %d Google Photos folders: %s", len(roots), strings.Join(roots, ", "))

	if same, err := sameVolume(roots[0], outputPath); err == nil && !same {
		err = fmt.Errorf("the output folder %s is on a different volume than the takeout. Files are only moved, not copied, so both have to be on the same drive", outputPath)
		Log(LoggerError, "%v", err)
		return err
	}

	// Exiftool is only needed to tell ambiguous sidecars apart and to restore .MOV extensions
	if err := InitializeExifTool(); err != nil {
		if options.RestoreMOVExtension {
			Log(LoggerError, "Failed to initialize exiftool: %v", err)
			return err
		}
		Log(LoggerWarn, "Exiftool not available, ambiguous sidecars are matched by name only: %v", err)
	} else {
		defer CloseExifTool()
	}

	dirs, err := collectSourceDirs(roots, options)
	if err != nil {
		Log(LoggerError, "%v", err)
		return err
	}

	p := Progress{}
	for _, dir := range dirs {
		p.Total += CountMediaFiles(dir.path)
	}
	progressCh <- p

	fixerCtx := &FixerContext{Ctx: ctx, SourceRoot: sourcePath, SourceRoots: roots, OutputRoot: outputPath, Options: options, ProgressCh: progressCh}
	org := &organizer{ctx: fixerCtx, dryRun: options.DryRun, planned: map[string]string{}, moved: map[string]bool{}, sidecars: map[string]bool{}, borrowed: map[string]string{}, inOutput: map[string]bool{}}
	if !options.DryRun {
		org.journal, err = OpenJournal(outputPath, "organize")
		if err != nil {
			Log(LoggerError, "Could not create the journal: %v", err)
			return err
		}
		defer org.journal.Close()
		Log(LoggerInfo, "Journal for undoing this run: %s", org.journal.Path)
		if err := org.journal.Record(JournalEntry{Op: "output", Path: outputPath}); err != nil {
			return err
		}
	}

	for _, dir := range dirs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := org.organizeDir(dir, &p); err != nil {
			Log(LoggerError, "Error organizing %s: %v", dir.path, err)
			return err
		}
	}

	if err := org.sweepRootFiles(roots); err != nil {
		Log(LoggerError, "%v", err)
		return err
	}
	if err := org.sweepLeftovers(roots, dirs); err != nil {
		Log(LoggerError, "%v", err)
		return err
	}
	org.removeEmptyFolders(roots, sourcePath, outputPath)

	s := org.stats
	if options.DryRun {
		Log(LoggerInfo, "Dry run: nothing was changed, the summary shows what would happen")
	}
	Log(LoggerInfo, "Summary: %d media files moved (%d renamed with a counter, %d given their real extension), %d sidecars moved", s.moved, s.renamed, s.extRenamed, s.sidecars)
	if s.duplicates > 0 {
		Log(LoggerInfo, "Summary: %d identical duplicates moved to %s", s.duplicates, filepath.Join(outputPath, duplicatesFolder))
	}
	if s.unmatched > 0 {
		Log(LoggerWarn, "Summary: %d sidecars without a media file moved to %s", s.unmatched, filepath.Join(outputPath, unmatchedFolder))
	}
	if s.other > 0 {
		Log(LoggerWarn, "Summary: %d other files moved to %s", s.other, filepath.Join(outputPath, unsortedFolder))
	}
	if s.dirsRemoved > 0 {
		Log(LoggerInfo, "Summary: %d empty folders removed", s.dirsRemoved)
	}
	if org.journal != nil {
		Log(LoggerInfo, "To undo this run: --undo \"%s\"", org.journal.Path)
	}
	return nil
}

// Moves the files of one takeout folder. Files are handled in groups that share a base name,
// so a photo, its edited copy, its live photo video and their sidecars get the same counter
func (org *organizer) organizeDir(dir sourceDir, p *Progress) error {
	entries, err := os.ReadDir(dir.path)
	if err != nil {
		return err
	}

	// The same folder in all takeouts, where sidecars of this folder's files may be
	var sidecarDirs []string
	for _, root := range org.ctx.SourceRoots {
		sidecarDirs = append(sidecarDirs, filepath.Join(root, dir.name))
	}

	groups := map[string][]string{}
	var order []string
	var metadataFiles, otherFiles []string
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir.path, name)
		switch {
		case entry.IsDir():
			Log(LoggerWarn, "Skipping nested folder %s, only files directly inside album and year folders are organized", path)
		case strings.HasPrefix(name, "."), strings.HasSuffix(name, "_original"):
			// Finder and Explorer artifacts and exiftool backups stay where they are
		case IsMediaFile(name):
			key := parseMediaName(name).base
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], path)
		case strings.EqualFold(name, "metadata.json"):
			metadataFiles = append(metadataFiles, path)
		case IsNameExtension(".json", path):
			// Sidecars travel with the media file they belong to, see sweepLeftovers for the rest
		default:
			otherFiles = append(otherFiles, path)
		}
	}
	sort.Strings(order)

	for _, key := range order {
		if org.ctx.Ctx.Err() != nil {
			return org.ctx.Ctx.Err()
		}
		if err := org.organizeGroup(dir, groups[key], sidecarDirs, p); err != nil {
			return err
		}
	}

	// The album's own metadata file: the first takeout's copy is kept
	for _, path := range metadataFiles {
		dest := filepath.Join(org.ctx.OutputRoot, dir.name, "metadata.json")
		if org.ctx.Options.Flatten {
			dest = filepath.Join(org.ctx.OutputRoot, dir.name+" metadata.json")
		}
		if existing, exists := org.exists(dest); exists {
			if SameFileContent(existing, path) {
				org.stats.duplicates++
			}
			if err := org.moveAside(path, duplicatesFolder, dir.name); err != nil {
				return err
			}
			continue
		}
		if err := org.move(path, dest); err != nil {
			return err
		}
	}

	for _, path := range otherFiles {
		Log(LoggerWarn, "Unsupported file %s, moving it to %s", path, unsortedFolder)
		if err := org.moveAside(path, unsortedFolder, dir.name); err != nil {
			return err
		}
		org.stats.other++
	}
	return nil
}

// Order in which the files of a group look for their sidecar: the original photo first, then
// videos (live photo clips borrow the photo's sidecar), then edited copies. Borrowers reuse the
// sidecar an original found instead of searching again, which could pick up another file's sidecar
func groupRank(path string) int {
	name := filepath.Base(path)
	switch {
	case IsVideoFile(name):
		return 1
	case splitCounterAndEdited(name):
		return 2
	default:
		return 0
	}
}

// Whether a file name carries an edited suffix like "-edited" or "-EFFECTS"
func splitCounterAndEdited(name string) bool {
	lower := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	plain, _ := splitDuplicateCounter(lower)
	for _, suffix := range editedSuffixes {
		if strings.HasSuffix(plain, suffix) {
			return true
		}
	}
	return false
}

// Moves the media files that share a base name, and the sidecars they own
func (org *organizer) organizeGroup(dir sourceDir, mediaPaths []string, sidecarDirs []string, p *Progress) error {
	sort.SliceStable(mediaPaths, func(i, j int) bool {
		return groupRank(mediaPaths[i]) < groupRank(mediaPaths[j])
	})

	// Sidecars found by the originals of this group, by parsed media name and counter
	groupSidecars := map[mediaName]string{}

	var moves []plannedMove
	for _, src := range mediaPaths {
		name := filepath.Base(src)

		// See issue #2
		if org.ctx.Options.RestoreMOVExtension && strings.EqualFold(filepath.Ext(name), ".mp4") {
			if majorBrand, err := GetMajorBrand(src); err == nil && strings.HasPrefix(majorBrand, "Apple QuickTime") {
				name = strings.TrimSuffix(name, filepath.Ext(name)) + ".mov"
				if filepath.Ext(filepath.Base(src)) == ".MP4" {
					name = strings.TrimSuffix(name, ".mov") + ".MOV"
				}
			}
		}
		if corrected := CorrectedFileName(src); corrected != filepath.Base(src) {
			Log(LoggerWarn, "%s is not a %s file but a %s, renaming it to %s", src, strings.ToUpper(strings.TrimPrefix(filepath.Ext(src), ".")), strings.ToUpper(strings.TrimPrefix(filepath.Ext(corrected), ".")), corrected)
			name = corrected
			org.stats.extRenamed++
		}

		parsed := parseMediaName(filepath.Base(src))
		// Without month subfolders the output folder does not depend on the sidecar
		destDir, err := ResolveOutputDir(org.ctx, src, "", dir.name, dir.isYearFolder)
		if err != nil {
			return err
		}
		// Sidecars that already moved with another file are not available anymore. Edited copies
		// and videos look for a sidecar of their own first, Google exports one for some of them
		sidecar, err := FindSidecarInExcluding(sidecarDirs, src, org.sidecars)
		if err != nil {
			return err
		}
		if sidecar == "" && IsVideoFile(src) {
			if partner, _ := FindImagePartner(src); partner != "" {
				sidecar, _ = FindSidecarInExcluding(sidecarDirs, partner, org.sidecars)
			}
		}
		if sidecar == "" && groupRank(src) > 0 {
			// The original of this group already took the shared sidecar, borrow it
			sidecar = groupSidecars[mediaName{name: parsed.name, base: parsed.base, index: parsed.index}]
			if sidecar == "" {
				// Live photo video of a photo with another extension, or an edited copy of a numbered photo
				for key, path := range groupSidecars {
					if key.base == parsed.base && key.index == parsed.index {
						sidecar = path
						break
					}
				}
			}
		}
		if sidecar != "" && org.ctx.Options.MonthSubfolders {
			// The month comes from the sidecar
			if destDir, err = ResolveOutputDir(org.ctx, src, sidecar, dir.name, dir.isYearFolder); err != nil {
				return err
			}
		}
		if sidecar == "" {
			// The sidecar may already have moved with the original or an identical copy from another
			// takeout, then this file finds it in the output folder later on
			if org.inOutput[strings.ToLower(filepath.Join(destDir, parsed.base))] {
				Log(LoggerInfo, "Sidecar for %s is already in %s", src, destDir)
			} else {
				Log(LoggerWarn, "No sidecar file found for %s", src)
			}
		} else if groupRank(src) == 0 {
			groupSidecars[mediaName{name: parsed.name, base: parsed.base, index: parsed.index}] = sidecar
		}

		moves = append(moves, plannedMove{src: src, sidecar: sidecar, destDir: destDir, name: name})
	}

	// Files that are already in the output with the same name and content are duplicates
	var remaining []int
	for i := range moves {
		dest := filepath.Join(moves[i].destDir, moves[i].name)
		if existing, exists := org.exists(dest); exists && SameFileContent(existing, moves[i].src) {
			moves[i].duplicate = existing
		} else {
			remaining = append(remaining, i)
		}
	}

	// Find a counter that lets all remaining files of the group move without overwriting anything
	counter := 0
	for {
		collision := false
		for _, i := range remaining {
			if _, exists := org.exists(filepath.Join(moves[i].destDir, moves[i].nameWithCounter(counter))); exists {
				collision = true
				break
			}
		}
		if !collision {
			break
		}
		counter++
	}
	if counter > 0 {
		Log(LoggerWarn, "%s is already used by a different file in %s, renaming %d files to \"(%d)\"", moves[remaining[0]].name, moves[remaining[0]].destDir, len(remaining), counter)
	}

	for _, pm := range moves {
		p.Processed++
		p.Current = pm.src

		if pm.duplicate != "" {
			Log(LoggerInfo, "%s is identical to %s, moving it to %s", pm.src, pm.duplicate, duplicatesFolder)
			if err := org.moveAside(pm.src, duplicatesFolder, dir.name); err != nil {
				return err
			}
			org.stats.duplicates++
			if pm.sidecar != "" && !org.sidecars[pm.sidecar] && ownsSidecar(pm.src, pm.sidecar) {
				if err := org.moveAside(pm.sidecar, duplicatesFolder, dir.name); err != nil {
					return err
				}
				org.sidecars[pm.sidecar] = true
			}
			org.ctx.ProgressCh <- *p
			continue
		}

		finalName := pm.nameWithCounter(counter)
		if err := org.move(pm.src, filepath.Join(pm.destDir, finalName)); err != nil {
			return err
		}
		org.stats.moved++
		if counter > 0 {
			org.stats.renamed++
		}
		if pm.sidecar != "" && !ownsSidecar(pm.src, pm.sidecar) {
			if _, ok := org.borrowed[pm.sidecar]; !ok {
				org.borrowed[pm.sidecar] = pm.destDir
			}
		}

		if pm.sidecar != "" && !org.sidecars[pm.sidecar] && ownsSidecar(pm.src, pm.sidecar) {
			// The sidecar keeps its name unless its media file was renamed, then it is named after the new name
			sidecarName := filepath.Base(pm.sidecar)
			if finalName != filepath.Base(pm.src) {
				sidecarName = finalName + ".supplemental-metadata.json"
			}
			dest := filepath.Join(pm.destDir, sidecarName)
			if _, exists := org.exists(dest); exists {
				Log(LoggerWarn, "Sidecar %s already exists in the output, moving %s to %s instead", dest, pm.sidecar, unmatchedFolder)
				if err := org.moveAside(pm.sidecar, unmatchedFolder, dir.name); err != nil {
					return err
				}
				org.stats.unmatched++
			} else {
				if err := org.move(pm.sidecar, dest); err != nil {
					return err
				}
				org.stats.sidecars++
				org.inOutput[strings.ToLower(filepath.Join(pm.destDir, parseMediaName(finalName).base))] = true
			}
			org.sidecars[pm.sidecar] = true
		}
		org.ctx.ProgressCh <- *p
	}
	return nil
}

// The output must not be inside the takeout and the takeout not inside the output, otherwise
// files would be moved onto themselves or picked up again as input
func checkOrganizePaths(sourcePath string, outputPath string) error {
	source, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	output, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(source); err == nil {
		source = resolved
	}
	if resolved, err := filepath.EvalSymlinks(output); err == nil {
		output = resolved
	}
	inside := func(path string, dir string) bool {
		return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
	}
	if inside(output, source) {
		return fmt.Errorf("the output folder %s is inside the takeout folder %s, choose a folder next to it instead", outputPath, sourcePath)
	}
	if inside(source, output) {
		return fmt.Errorf("the takeout folder %s is inside the output folder %s", sourcePath, outputPath)
	}
	return nil
}

// Google also exports a few files directly into the Google Photos folder, like
// "user-generated-memory-titles.json" or "print-subscriptions.json". They go to the unsorted
// folder so the takeout folders can be removed once they are empty
func (org *organizer) sweepRootFiles(roots []string) error {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if err := org.moveAside(filepath.Join(root, entry.Name()), unsortedFolder, filepath.Base(root)); err != nil {
				return err
			}
			org.stats.other++
		}
	}
	return nil
}

// Finds the output folder that holds a media file the sidecar belongs to, or "". After an
// interrupted run the media file may already be in the output while its sidecar is still in the takeout
func (org *organizer) ownerFolderInOutput(sidecarPath string, folderName string) string {
	sidecarName, _, ok := parseSidecarName(filepath.Base(sidecarPath))
	if !ok || !IsMediaFile(sidecarName) {
		return ""
	}
	base := parseMediaName(sidecarName).base
	for _, dir := range []string{filepath.Join(org.ctx.OutputRoot, folderName), filepath.Join(org.ctx.OutputRoot, duplicatesFolder, folderName)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && IsMediaFile(entry.Name()) && parseMediaName(entry.Name()).base == base {
				return dir
			}
		}
	}
	return ""
}

// Moves sidecars that no media file claimed to the unmatched folder, so they are not lost
func (org *organizer) sweepLeftovers(roots []string, dirs []sourceDir) error {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(dir.path, name)
			if entry.IsDir() || !IsNameExtension(".json", name) || org.sidecars[path] || org.moved[path] {
				continue
			}
			// The media file may have moved to the output in an earlier, interrupted run
			if !org.ctx.Options.Flatten {
				if ownerDir := org.ownerFolderInOutput(path, dir.name); ownerDir != "" {
					dest := org.freePath(ownerDir, name)
					if filepath.Base(dest) != name {
						Log(LoggerWarn, "Sidecar %s already exists next to its media file, keeping the extra copy as %s", name, filepath.Base(dest))
					}
					if err := org.move(path, dest); err != nil {
						return err
					}
					org.sidecars[path] = true
					org.stats.sidecars++
					continue
				}
			}
			// A sidecar only used by a live photo video or edited copy whose original is not in the
			// takeout goes along with that file, so it still finds its metadata
			if destDir, ok := org.borrowed[path]; ok {
				dest := filepath.Join(destDir, name)
				if _, exists := org.exists(dest); !exists {
					if err := org.move(path, dest); err != nil {
						return err
					}
					org.sidecars[path] = true
					org.stats.sidecars++
					continue
				}
			}
			Log(LoggerWarn, "No media file found for sidecar %s, moving it to %s", path, unmatchedFolder)
			if err := org.moveAside(path, unmatchedFolder, dir.name); err != nil {
				return err
			}
			org.stats.unmatched++
		}
	}
	return nil
}

// Removes the takeout folders that are now empty, from the year and album folders up to the
// archive folders. The source folder itself and the output folder are never removed
func (org *organizer) removeEmptyFolders(roots []string, sourcePath string, outputPath string) {
	if org.dryRun {
		Log(LoggerInfo, "[dry-run] Empty takeout folders would be removed")
		return
	}
	for _, root := range roots {
		subdirs, _ := DiscoverDirs(root)
		for _, sub := range subdirs {
			org.removeIfEmpty(filepath.Join(root, sub.Name()), sourcePath, outputPath)
		}
		for dir := root; dir != sourcePath && strings.HasPrefix(dir, sourcePath); dir = filepath.Dir(dir) {
			if !org.removeIfEmpty(dir, sourcePath, outputPath) {
				break
			}
		}
	}
}

func (org *organizer) removeIfEmpty(dir string, sourcePath string, outputPath string) bool {
	if dir == sourcePath || dir == outputPath || strings.HasPrefix(outputPath, dir+string(filepath.Separator)) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return false
	}
	if err := os.Remove(dir); err != nil {
		return false
	}
	if err := org.journal.Record(JournalEntry{Op: "rmdir", Path: dir}); err != nil {
		Log(LoggerWarn, "Removed empty folder %s but could not write the journal: %v", dir, err)
	}
	org.stats.dirsRemoved++
	return true
}
