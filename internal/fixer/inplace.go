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
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The tags the in-place mode may change, snapshotted before and after every write so the
// previous state can be restored. Group names make the values round-trip exactly
var journaledTags = []string{
	"-time:all", "-Title", "-Description", "-ImageDescription", "-Caption-Abstract",
	"-PersonInImage", "-Subject", "-Rating", "-GPS:all", "-Keys:GPSCoordinates", "-UserData:GPSCoordinates",
	"-Keys:Description", "-XMP-exif:GPS*",
}

// Reads the current values of the journaled tags, keyed by "Group1:Tag"
func snapshotTags(filePath string) (map[string]string, error) {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return nil, fmt.Errorf("Exiftool is not initialized")
	}

	// -n keeps numbers exact, so GPS values survive the round trip through the journal
	args := append([]string{"-j", "-G1", "-a", "-n", "-sep", "||", "-charset", "filename=utf8"}, journaledTags...)
	lines, err := runExifTool(append(args, filePath))
	if err != nil {
		return nil, err
	}

	var decoded []map[string]any
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &decoded); err != nil || len(decoded) == 0 {
		return nil, fmt.Errorf("could not read the metadata of %s", filePath)
	}

	snapshot := map[string]string{}
	for key, value := range decoded[0] {
		// Only the file dates of the System group can be restored, access and inode change dates cannot
		if key == "SourceFile" || strings.HasPrefix(key, "File:") || strings.HasPrefix(key, "Composite:") ||
			(strings.HasPrefix(key, "System:") && key != "System:FileModifyDate" && key != "System:FileCreateDate") {
			continue
		}
		snapshot[key] = fmt.Sprint(value)
	}
	return snapshot, nil
}

// The journal entry for a change: only tags whose value changed are recorded, so undoing writes
// back as little as possible. Tags the change added are removed again by Undo
func changedTags(path string, before map[string]string, after map[string]string, modTime time.Time, backup string) JournalEntry {
	changed := map[string]string{}
	var added []string
	for tag, value := range before {
		if after[tag] != value {
			changed[tag] = value
		}
	}
	for tag := range after {
		if _, existed := before[tag]; !existed {
			added = append(added, tag)
		}
	}
	sort.Strings(added)
	return JournalEntry{Op: "metadata", Path: path, Before: changed, Added: added, ModTime: modTime.Format(time.RFC3339), Backup: backup}
}

// Modification time of a file, for the journal
func modTimeOf(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// Restores the metadata of a file from a journal entry. If exiftool's backup copy was kept,
// the backup simply replaces the file, which restores it byte for byte. Otherwise every
// journaled tag is written back and tags the run added are removed
func undoMetadata(entry JournalEntry) error {
	if entry.Backup != "" {
		if _, err := os.Stat(entry.Backup); err == nil {
			return os.Rename(entry.Backup, entry.Path)
		}
	}

	// No -m here: ignoring minor errors would let exiftool delete trailing data it does not understand
	args := []string{"-overwrite_original", "-E", "-n", "-sep", "||", "-charset", "filename=utf8"}
	for _, tag := range entry.Added {
		if _, existed := entry.Before[tag]; !existed {
			args = append(args, "-"+tag+"=")
		}
	}
	keys := make([]string, 0, len(entry.Before))
	for tag := range entry.Before {
		keys = append(keys, tag)
	}
	sort.Strings(keys)
	for _, tag := range keys {
		args = append(args, "-"+tag+"="+escapeExifValue(entry.Before[tag]))
	}

	// Nothing but file dates changed: no need to touch the file's content
	if len(args) > 7 {
		exifToolMutex.Lock()
		_, err := runExifTool(append(args, entry.Path))
		exifToolMutex.Unlock()
		if err != nil {
			return err
		}
	}

	if modTime, parseErr := time.Parse(time.RFC3339, entry.ModTime); parseErr == nil {
		return os.Chtimes(entry.Path, modTime, modTime)
	}
	return nil
}

// ProcessInPlace writes the sidecar metadata into the media files where they are, without copying
// them, so no extra disk space is needed. Safety measures:
//
//   - exiftool writes a temporary file and renames it over the original, so a crash never leaves
//     a half-written file
//   - the previous values of every tag that is changed are recorded in a journal, and Undo writes
//     them back; with KeepBackups exiftool's "_original" copies are kept and Undo restores those,
//     which brings back the exact original bytes but needs the disk space for the copies
//   - every written date is read back and compared with what was intended
//
// Files with a wrong extension are renamed to their real type, which is journaled as a move
func ProcessInPlace(
	ctx context.Context,
	sourcePath string,
	progressCh chan<- Progress,
	options ProcessOptions,
) error {
	finish := startRun("in-place processing", sourcePath, sourcePath)
	defer finish()
	defer close(progressCh)

	if options.UseSymlinks || options.Flatten || options.MonthSubfolders {
		return fmt.Errorf("symlinks, flatten and month subfolders are not available when fixing files in place, organize the files first")
	}

	// A dry run uses exiftool only to read, so it may run without it
	if err := InitializeExifTool(); err != nil {
		if !options.DryRun {
			Log(LoggerError, "Failed to initialize exiftool: %v", err)
			return err
		}
		Log(LoggerWarn, "Exiftool not available, ambiguous sidecars are matched by name only: %v", err)
	} else {
		defer CloseExifTool()
	}

	roots, err := FindSourceRoots(sourcePath)
	if err != nil {
		Log(LoggerError, "Error finding Google Photos folders: %v", err)
		return err
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

	fixerCtx := &FixerContext{Ctx: ctx, SourceRoot: sourcePath, SourceRoots: roots, OutputRoot: sourcePath, Options: options, ProgressCh: progressCh}
	var journal *Journal
	if !options.DryRun {
		journal, err = OpenJournal(sourcePath, "metadata")
		if err != nil {
			Log(LoggerError, "Could not create the journal: %v", err)
			return err
		}
		defer journal.Close()
		Log(LoggerInfo, "Journal for undoing this run: %s", journal.Path)
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			Log(LoggerError, "Error reading directory: %v", err)
			continue
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry.IsDir() || !IsMediaFile(entry.Name()) {
				continue
			}

			fixerCtx.Stats.Found.Add(1)
			p.Processed++
			p.Current = filepath.Join(dir.path, entry.Name())
			if err := fixFileInPlace(fixerCtx, journal, filepath.Join(dir.path, entry.Name())); err != nil {
				fixerCtx.Stats.Failed.Add(1)
				Log(LoggerError, "Error processing file %s: %v", p.Current, err)
			}
			progressCh <- p
		}
	}

	fixerCtx.Stats.LogSummary(options.DryRun)
	if journal != nil {
		Log(LoggerInfo, "To undo this run: --undo \"%s\"", journal.Path)
	}
	return nil
}

// Fixes one media file where it is
func fixFileInPlace(fixerCtx *FixerContext, journal *Journal, path string) error {
	options := fixerCtx.Options

	sidecarPath, err := fixerCtx.FindSidecar(path)
	if err != nil {
		return err
	}
	if sidecarPath == "" && IsVideoFile(path) {
		if partner, _ := FindImagePartner(path); partner != "" {
			sidecarPath, _ = fixerCtx.FindSidecar(partner)
		}
	}

	// Google exports some JPEGs as .png or .heic, which exiftool refuses to write to
	if corrected := CorrectedFileName(path); corrected != filepath.Base(path) {
		newPath := filepath.Join(filepath.Dir(path), corrected)
		if _, err := os.Lstat(newPath); err == nil {
			Log(LoggerWarn, "%s has the wrong extension but %s already exists, keeping the name", path, corrected)
		} else if options.DryRun {
			Log(LoggerInfo, "[dry-run] Would rename %s -> %s (real file type)", path, corrected)
		} else {
			Log(LoggerWarn, "%s is not a %s file but a %s, renaming it to %s", path, strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), ".")), strings.ToUpper(strings.TrimPrefix(filepath.Ext(corrected), ".")), corrected)
			if err := MoveFile(path, newPath); err != nil {
				return err
			}
			if err := journal.Record(JournalEntry{Op: "move", From: path, To: newPath}); err != nil {
				return err
			}
			ClearCacheDir(filepath.Dir(path))
			path = newPath
			fixerCtx.Stats.RenamedExtension.Add(1)
		}
	}

	if sidecarPath == "" {
		fixerCtx.Stats.NoSidecar.Add(1)
		Log(LoggerWarn, "No sidecar file found for %s", path)
		if options.DryRun || !options.WriteMetadata {
			fixerCtx.Stats.Written.Add(1)
			return nil
		}
		date, found := ReadExifDate(path)
		source := "its metadata"
		if !found {
			date, found = ParseDateFromFileName(filepath.Base(path))
			source = "its file name"
		}
		if !found {
			Log(LoggerInfo, "No date found for %s, leaving its file dates as they are", path)
			fixerCtx.Stats.Written.Add(1)
			return nil
		}
		before, err := snapshotTags(path)
		if err != nil {
			return err
		}
		modTime := modTimeOf(path)
		if err := SetFileDates(path, date); err != nil {
			fixerCtx.Stats.MetadataFailed.Add(1)
			return err
		}
		after, _ := snapshotTags(path)
		if err := journal.Record(changedTags(path, before, after, modTime, "")); err != nil {
			return err
		}
		Log(LoggerInfo, "No sidecar for %s, set file dates to %s from %s", filepath.Base(path), date.Format("2006-01-02 15:04:05"), source)
		fixerCtx.Stats.Written.Add(1)
		return nil
	}

	if options.DryRun {
		if err := CheckJsonMetadata(sidecarPath); err != nil {
			fixerCtx.Stats.MetadataFailed.Add(1)
			Log(LoggerError, "Failed to read metadata from %s: %v", sidecarPath, err)
		} else {
			Log(LoggerInfo, "[dry-run] Would apply metadata from %s to %s", sidecarPath, path)
		}
		fixerCtx.Stats.Written.Add(1)
		return nil
	}
	if !options.WriteMetadata {
		fixerCtx.Stats.Written.Add(1)
		return nil
	}

	metadata, err := ReadJsonMetadata(sidecarPath)
	if err != nil {
		fixerCtx.Stats.MetadataFailed.Add(1)
		return fmt.Errorf("failed to read metadata from %s: %w", sidecarPath, err)
	}

	args, utcTime, err := metadataArgs(path, filepath.Base(path), metadata)
	if err != nil {
		fixerCtx.Stats.MetadataFailed.Add(1)
		return err
	}

	before, err := snapshotTags(path)
	if err != nil {
		return err
	}
	modTime := modTimeOf(path)

	backup := ""
	if options.KeepBackups {
		// Without -overwrite_original exiftool keeps the original as "<name>_original"
		args = args[1:]
		backup = path + "_original"
	}

	exifToolMutex.Lock()
	_, writeErr := runExifTool(args)
	exifToolMutex.Unlock()

	after, snapshotErr := snapshotTags(path)
	if snapshotErr != nil {
		// Record everything, so Undo can still restore the previous values
		after = map[string]string{}
	}
	if _, err := os.Stat(backup); backup != "" && err != nil {
		backup = ""
	}
	if err := journal.Record(changedTags(path, before, after, modTime, backup)); err != nil {
		return err
	}

	if writeErr != nil {
		fixerCtx.Stats.MetadataFailed.Add(1)
		Log(LoggerError, "Failed to apply metadata to %s: %v", path, writeErr)
		explainMinorError(writeErr)
		if !utcTime.IsZero() {
			if dateErr := SetFileDates(path, utcTime); dateErr != nil {
				Log(LoggerError, "Failed to set file dates of %s: %v", path, dateErr)
			}
		}
		fixerCtx.Stats.Written.Add(1)
		return nil
	}

	if !utcTime.IsZero() {
		if err := os.Chtimes(path, utcTime, utcTime); err != nil {
			return fmt.Errorf("failed to set file timestamps: %w", err)
		}
	}

	// Read the date back to make sure the file says what the sidecar says
	if written := readExistingDate(path); written.found {
		expected := expectedWallClock(args)
		if expected != "" && written.wallClock.Format("2006:01:02 15:04:05") != expected {
			fixerCtx.Stats.MetadataFailed.Add(1)
			Log(LoggerError, "Verification failed for %s: wrote %s but the file now says %s", path, expected, written.wallClock.Format("2006:01:02 15:04:05"))
		}
	}

	fixerCtx.Stats.Written.Add(1)
	return nil
}

// The local time that the -AllDates argument writes
func expectedWallClock(args []string) string {
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, "-AllDates="); ok && len(value) >= 19 {
			return value[:19]
		}
	}
	return ""
}

// Coordinates Google exports are rounded to four decimals, about 11 metres. A camera's own
// coordinates are more precise, so an existing location this close to Google's is kept
const sameLocationMetres = 15.0

// Rough distance in metres between two coordinates, good enough to tell rounding from a move
func distanceMetres(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := (lat2 - lat1) * 111000
	dLon := (lon2 - lon1) * 111000 * math.Cos(lat1*math.Pi/180)
	return math.Sqrt(dLat*dLat + dLon*dLon)
}

// Signed latitude and longitude from a tag snapshot taken with -n, from whichever GPS tags the file has
func locationFromSnapshot(tags map[string]string) (float64, float64, bool) {
	if coords, ok := tags["Keys:GPSCoordinates"]; ok {
		if lat, lon, ok := parseCoordinates(coords); ok {
			return lat, lon, true
		}
	}
	if coords, ok := tags["UserData:GPSCoordinates"]; ok {
		if lat, lon, ok := parseCoordinates(coords); ok {
			return lat, lon, true
		}
	}
	for _, group := range []string{"GPS", "XMP-exif"} {
		lat, errLat := strconv.ParseFloat(tags[group+":GPSLatitude"], 64)
		lon, errLon := strconv.ParseFloat(tags[group+":GPSLongitude"], 64)
		if errLat != nil || errLon != nil {
			continue
		}
		// EXIF stores unsigned values with a hemisphere. With -n the reference is "S"/"W" or "N"/"E"
		if ref := tags[group+":GPSLatitudeRef"]; ref == "S" || ref == "South" {
			lat = -math.Abs(lat)
		}
		if ref := tags[group+":GPSLongitudeRef"]; ref == "W" || ref == "West" {
			lon = -math.Abs(lon)
		}
		return lat, lon, true
	}
	return 0, 0, false
}

// Parses "lat lon [alt]" as exiftool prints QuickTime coordinates with -n
func parseCoordinates(coords string) (float64, float64, bool) {
	parts := strings.Fields(coords)
	if len(parts) < 2 {
		return 0, 0, false
	}
	lat, errLat := strconv.ParseFloat(parts[0], 64)
	lon, errLon := strconv.ParseFloat(parts[1], 64)
	return lat, lon, errLat == nil && errLon == nil
}

// Reads the location a file has right now, signed
func readExistingLocation(filePath string) (float64, float64, bool) {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return 0, 0, false
	}
	lines, err := runExifTool([]string{"-n", "-s3", "-f", "-Composite:GPSLatitude", "-Composite:GPSLongitude", "-charset", "filename=utf8", filePath})
	if err != nil || len(lines) < 2 {
		return 0, 0, false
	}
	lat, errLat := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	lon, errLon := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	return lat, lon, errLat == nil && errLon == nil
}

// RestoreGPS puts the coordinates a file had before an in-place run back, using that run's journal,
// for every file whose location Google had only rounded (within maxMetres). Files whose location
// was really changed in Google Photos keep Google's. The restore is journaled like any other run
func RestoreGPS(journalPath string, maxMetres float64, dryRun bool) error {
	entries, err := ReadJournal(journalPath)
	if err != nil {
		return err
	}
	finish := startRun("restoring GPS precision", journalPath, filepath.Dir(journalPath))
	defer finish()

	if err := InitializeExifTool(); err != nil {
		return err
	}
	defer CloseExifTool()

	var journal *Journal
	if !dryRun {
		journal, err = OpenJournal(filepath.Dir(journalPath), "metadata")
		if err != nil {
			return err
		}
		defer journal.Close()
		Log(LoggerInfo, "Journal for undoing this run: %s", journal.Path)
	}

	restored, kept, unchanged, failed := 0, 0, 0, 0
	for _, entry := range entries {
		if entry.Op != "metadata" {
			continue
		}
		var gpsBefore []string
		for tag := range entry.Before {
			if strings.Contains(tag, "GPS") {
				gpsBefore = append(gpsBefore, tag)
			}
		}
		var gpsAdded []string
		for _, tag := range entry.Added {
			if strings.Contains(tag, "GPS") {
				gpsAdded = append(gpsAdded, tag)
			}
		}
		if len(gpsBefore) == 0 {
			continue
		}
		sort.Strings(gpsBefore)
		sort.Strings(gpsAdded)

		current, err := snapshotTags(entry.Path)
		if err != nil {
			failed++
			Log(LoggerError, "Could not read %s: %v", entry.Path, err)
			continue
		}
		curLat, curLon, hasCurrent := locationFromSnapshot(current)

		// The location before the run: the journaled tags, filled up with the unchanged ones
		previous := map[string]string{}
		for tag, value := range current {
			previous[tag] = value
		}
		for _, tag := range gpsAdded {
			delete(previous, tag)
		}
		for _, tag := range gpsBefore {
			previous[tag] = entry.Before[tag]
		}
		prevLat, prevLon, hasPrevious := locationFromSnapshot(previous)

		if hasCurrent && hasPrevious {
			if distance := distanceMetres(prevLat, prevLon, curLat, curLon); distance > maxMetres {
				kept++
				Log(LoggerInfo, "Keeping Google's location for %s, it is %.0f m from the original (%f, %f -> %f, %f)", entry.Path, distance, prevLat, prevLon, curLat, curLon)
				continue
			}
		}

		args := []string{"-overwrite_original", "-E", "-n", "-sep", "||", "-charset", "filename=utf8"}
		for _, tag := range gpsAdded {
			args = append(args, "-"+tag+"=")
		}
		for _, tag := range gpsBefore {
			args = append(args, "-"+tag+"="+escapeExifValue(entry.Before[tag]))
		}

		if dryRun {
			restored++
			Log(LoggerInfo, "[dry-run] Would restore %s on %s", strings.Join(append(gpsBefore, gpsAdded...), ", "), entry.Path)
			continue
		}

		modTime := modTimeOf(entry.Path)
		exifToolMutex.Lock()
		_, writeErr := runExifTool(append(args, entry.Path))
		exifToolMutex.Unlock()
		if writeErr != nil {
			failed++
			Log(LoggerError, "Could not restore GPS of %s: %v", entry.Path, writeErr)
			continue
		}
		// The write changes the modification time, the dates of the run stay
		if err := os.Chtimes(entry.Path, modTime, modTime); err != nil {
			Log(LoggerWarn, "Could not keep the modification time of %s: %v", entry.Path, err)
		}

		after, _ := snapshotTags(entry.Path)
		change := changedTags(entry.Path, current, after, modTime, "")
		if len(change.Before) == 0 && len(change.Added) == 0 {
			unchanged++
		} else if err := journal.Record(change); err != nil {
			return err
		}
		restored++
	}

	if dryRun {
		Log(LoggerInfo, "Dry run: nothing was changed, the summary shows what would happen")
	}
	Log(LoggerInfo, "Summary: original GPS coordinates restored on %d files (%d of them turned out identical), Google's location kept on %d files that were moved further than %.0f m", restored, unchanged, kept, maxMetres)
	if failed > 0 {
		Log(LoggerError, "Summary: %d files could not be processed, see the log", failed)
	}
	if journal != nil {
		Log(LoggerInfo, "To undo this run: --undo \"%s\"", journal.Path)
	}
	return nil
}
