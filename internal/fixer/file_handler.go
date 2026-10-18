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
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache for diectory entries to prevent excessive disk reads (issue #5)
var (
	dirCache     = make(map[string][]os.DirEntry)
	dirCacheLock sync.RWMutex
)

// ReadDirCached returns cached directories or reads them it not present
func ReadDirCached(dir string) ([]os.DirEntry, error) {
	dirCacheLock.RLock()
	entries, ok := dirCache[dir]
	dirCacheLock.RUnlock()

	// Cache hit, return entries
	if ok {
		return entries, nil
	}

	dirCacheLock.Lock()
	defer dirCacheLock.Unlock()

	// Check again in case it was created while waiting for lock
	if entries, ok = dirCache[dir]; ok {
		return entries, nil
	}

	// Read directory and cache results
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	dirCache[dir] = entries

	return entries, nil
}

// ClearCache clears the directory cache for all paths
func ClearCache() {
	dirCacheLock.Lock()
	defer dirCacheLock.Unlock()
	// Reallocate map to clear everything
	dirCache = make(map[string][]os.DirEntry)

	claimedSidecarsLock.Lock()
	defer claimedSidecarsLock.Unlock()
	claimedSidecars = make(map[string]string)
}

// ClearCacheDir clears the directory cache for a specific path
func ClearCacheDir(dir string) {
	dirCacheLock.Lock()
	defer dirCacheLock.Unlock()
	delete(dirCache, dir)
}

// All media extension to differ between media files and other files
// I could image that google does some renaming like with .mov (see issue #2)
var imageExtensions = []string{
	".jpg",
	".jpeg",
	".png",
	".gif",
	".webp",
	".bmp",
	".arw",
	".cr2",
	".cr3",
	".tif",
	".tiff",
	".heic",
	".heif",
	".dng",
	".raf",
	".rw2",
	".srw",
	".nef",
	".nrw",
	".orf",
}

var videoExtensions = []string{
	".mp4",
	".m4v",
	".mov",
	".mpeg",
	".m2ts",
	".webm",
	".wmv",
	".avi",
	".mkv",
	".3gp",
	".3g2",
	".mpg",
	".mts",
	".mp", // Video part of Google Pixel motion photos
}

// Checks whether a file is a video file based on its extension
func IsVideoFile(path string) bool {
	extension := filepath.Ext(path)
	return slices.Contains(videoExtensions, strings.ToLower(extension))
}

// Duplicate a file from one path to another
func DuplicateFile(inputPath string, outputPath string) error {
	sourceFile, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

// Checks whether two files have exactly the same content
func SameFileContent(pathA string, pathB string) bool {
	infoA, errA := os.Stat(pathA)
	infoB, errB := os.Stat(pathB)
	if errA != nil || errB != nil || infoA.Size() != infoB.Size() {
		return false
	}
	if os.SameFile(infoA, infoB) {
		return true
	}

	fileA, err := os.Open(pathA)
	if err != nil {
		return false
	}
	defer fileA.Close()

	fileB, err := os.Open(pathB)
	if err != nil {
		return false
	}
	defer fileB.Close()

	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		nA, errA := io.ReadFull(fileA, bufA)
		nB, errB := io.ReadFull(fileB, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false
		}
		if errA == io.EOF || errA == io.ErrUnexpectedEOF {
			return errB == io.EOF || errB == io.ErrUnexpectedEOF
		}
		if errA != nil || errB != nil {
			return false
		}
	}
}

// Discover directories within a path non recursively
func DiscoverDirs(path string) ([]os.DirEntry, error) {
	var dirList []os.DirEntry

	files, err := os.ReadDir(path)

	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if file.IsDir() {
			dirList = append(dirList, file)
		}
	}

	return dirList, nil
}

// Google appends "(1)", "(2)", ... to files that share the same name (issue #28)
var duplicateCounterRegex = regexp.MustCompile(`\((\d+)\)$`)

// Suffixes Google adds to edited copies of a file. Edited copies have no sidecar of their own
// and use the sidecar of the original file. Localized versions taken from Google Photos Takeout Helper
var editedSuffixes = []string{
	"-edited", "-effects", "-smile", "-mix", // English
	"-bearbeitet", // German
	"-modifié",    // French
	"-modificato", // Italian
	"-ha editado", // Spanish
	"-editat",     // Catalan
	"-bewerkt",    // Dutch
	"-edytowane",  // Polish
	"-編集済み",       // Japanese
}

// Newer takeouts name sidecars "<file>.supplemental-metadata.json", but Google cuts the
// suffix to keep the sidecar name short, e.g. "<file>.supplemental-me.json" or "<file>.s.json"
const supplementalMetadataSuffix = "supplemental-metadata"

// Google limits sidecar file names to 51 characters. Sidecars this long may have their
// media file name cut off, e.g. "a_very_long_file_name_that_goes_on_and_on_f.json"
const truncatedSidecarNameLength = 46

// mediaName holds the parts of a media file name that are needed to find its sidecar
type mediaName struct {
	name  string // lowercased file name without duplicate counter and edited suffix, e.g. "img_0001.jpg"
	base  string // name without extension, e.g. "img_0001"
	index int    // duplicate counter, 0 if there is none
}

// Gives a file name the n-th duplicate counter: "IMG_0001.jpg" -> "IMG_0001(1).jpg". A name that
// already carries a counter continues counting instead of stacking: "IMG_0001(1).jpg" -> "IMG_0001(2).jpg"
func NameWithCounter(fileName string, n int) string {
	if n <= 0 {
		return fileName
	}
	ext := filepath.Ext(fileName)
	stem, existing := splitDuplicateCounter(strings.TrimSuffix(fileName, ext))
	return fmt.Sprintf("%s(%d)%s", stem, existing+n, ext)
}

// Removes a trailing "(n)" duplicate counter and returns it, or 0 if there is none.
// Google's counters start at 1, a "(0)" is part of the name (e.g. Samsung burst photos)
func splitDuplicateCounter(s string) (string, int) {
	match := duplicateCounterRegex.FindStringSubmatchIndex(s)
	if match == nil {
		return s, 0
	}
	index, err := strconv.Atoi(s[match[2]:match[3]])
	if err != nil || index == 0 {
		return s, 0
	}
	return strings.TrimRight(s[:match[0]], " "), index
}

// Removes edited suffixes like "-edited" or "-EFFECTS" from a lowercased name stem.
// Suffixes can stack, e.g. an edited copy of an effects version: "IMG_0001-EFFECTS-edited"
func stripEditedSuffix(stem string) (string, bool) {
	stripped := false
	for again := true; again; {
		again = false
		for _, suffix := range editedSuffixes {
			if trimmed, ok := strings.CutSuffix(stem, suffix); ok {
				stem, stripped, again = trimmed, true, true
				break
			}
		}
	}
	return stem, stripped
}

// The parts of a media file name that matter for finding its sidecar
type nameParts struct {
	lower         string // "img_0001-edited(1).jpg"
	ext           string // ".jpg"
	stemNoCounter string // "img_0001-edited", the name Google numbered
	trailing      int    // 1, the number Google appended to that name
	edited        bool
}

func splitName(fileName string) nameParts {
	lower := strings.ToLower(fileName)
	ext := filepath.Ext(lower)
	stemNoCounter, trailing := splitDuplicateCounter(strings.TrimSuffix(lower, ext))
	_, edited := stripEditedSuffix(stemNoCounter)
	return nameParts{lower: lower, ext: ext, stemNoCounter: stemNoCounter, trailing: trailing, edited: edited}
}

// Splits a media file name into the name of the original photo and its duplicate counter:
//
//	"IMG_0001.jpg"           -> img_0001.jpg, 0
//	"IMG_0001(1).jpg"        -> img_0001.jpg, 1   (a different photo with the same name)
//	"IMG_0001-edited.jpg"    -> img_0001.jpg, 0   (edited copy of IMG_0001.jpg)
//	"IMG_0001(1)-edited.jpg" -> img_0001.jpg, 1   (edited copy of IMG_0001(1).jpg)
//	"IMG_0001-edited(1).jpg" -> img_0001.jpg, 0   (second edited copy of IMG_0001.jpg)
func parseMediaName(fileName string) mediaName {
	parts := splitName(fileName)
	base, edited := stripEditedSuffix(parts.stemNoCounter)
	index := parts.trailing
	if edited {
		// A counter after the edited suffix numbers the edited copy, only a counter in front of it
		// belongs to the original
		base, index = splitDuplicateCounter(base)
	}
	return mediaName{name: base + parts.ext, base: base, index: index}
}

// Parses a sidecar file name and returns the lowercased media file name it belongs to
// and its duplicate counter. These all belong to "img_0001.jpg" with counter 1:
// "IMG_0001.jpg(1).json", "IMG_0001.jpg.supplemental-metadata(1).json", "IMG_0001(1).jpg.json"
func parseSidecarName(fileName string) (string, int, bool) {
	name, ok := strings.CutSuffix(strings.ToLower(fileName), ".json")
	if !ok {
		return "", 0, false
	}

	name, index := splitDuplicateCounter(name)

	// Strip the (possibly cut off) supplemental-metadata suffix
	if dot := strings.LastIndex(name, "."); dot != -1 {
		if strings.HasPrefix(supplementalMetadataSuffix, name[dot+1:]) {
			name = name[:dot]
		}
	}

	// Counter might be placed in front of the extension, e.g. "IMG_0001(1).jpg.json"
	if index == 0 {
		ext := filepath.Ext(name)
		var base string
		base, index = splitDuplicateCounter(strings.TrimSuffix(name, ext))
		name = base + ext
	}

	return name, index, true
}

// How well a sidecar name fits a media name, lower is better
const (
	matchExact       = iota // "IMG_0001.jpg.json" for "IMG_0001.jpg"
	matchWithoutExt         // Older takeouts use "IMG_0001.json"
	matchSameBase           // Same file with another extension, e.g. the video of a live photo or a renamed .mov (see issue #2)
	matchMotionPhoto        // The video part of a Pixel motion photo "PXL_0001.MP" uses the sidecar of "PXL_0001.MP.jpg"
	matchTruncated          // Google cut the sidecar name short
)

type sidecarMatch struct {
	path string
	rank int
}

// Searches the directory entries for sidecars that could belong to the given media name.
// With exactOnly only a sidecar named after the full media name is accepted
func matchSidecars(dir string, entries []os.DirEntry, media mediaName, exactOnly bool) []sidecarMatch {
	var matches []sidecarMatch
	var truncated string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		sidecarName, index, ok := parseSidecarName(entry.Name())
		if !ok || index != media.index {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if sidecarName == media.name {
			matches = append(matches, sidecarMatch{path, matchExact})
			continue
		}
		if exactOnly {
			continue
		}

		sidecarBase := strings.TrimSuffix(sidecarName, filepath.Ext(sidecarName))
		switch {
		case sidecarName == media.base:
			matches = append(matches, sidecarMatch{path, matchWithoutExt})
		case IsMediaFile(sidecarName) && sidecarBase == media.base:
			matches = append(matches, sidecarMatch{path, matchSameBase})
		case IsMediaFile(sidecarName) && sidecarBase == media.name:
			matches = append(matches, sidecarMatch{path, matchMotionPhoto})
		case len(entry.Name()) >= truncatedSidecarNameLength && strings.HasPrefix(media.name, sidecarName):
			// Prefer the longest match in case multiple shortened sidecars fit
			if truncated == "" || len(entry.Name()) > len(filepath.Base(truncated)) {
				truncated = path
			}
		}
	}

	if truncated != "" {
		matches = append(matches, sidecarMatch{truncated, matchTruncated})
	}
	return matches
}

// Sidecars already assigned to a media file during this run, used to resolve ambiguous matches
var (
	claimedSidecars     = make(map[string]string)
	claimedSidecarsLock sync.Mutex
)

func claimSidecar(sidecarPath string, mediaPath string) {
	claimedSidecarsLock.Lock()
	defer claimedSidecarsLock.Unlock()
	if _, ok := claimedSidecars[sidecarPath]; !ok {
		claimedSidecars[sidecarPath] = mediaPath
	}
}

// Whether all sidecars carry the same title, taken time, location and description
func sameSidecarMetadata(paths []string) bool {
	first, err := ReadJsonMetadata(paths[0])
	if err != nil {
		return false
	}
	for _, path := range paths[1:] {
		other, err := ReadJsonMetadata(path)
		if err != nil || other.Title != first.Title || other.Description != first.Description ||
			other.PhotoTakenTime.Timestamp != first.PhotoTakenTime.Timestamp || other.GeoData != first.GeoData {
			return false
		}
	}
	return true
}

// Picks the sidecar for a media file out of several possible ones. Candidates with a better
// name match win. Among equally good names, e.g. the sidecars of "IMG_0001.HEIC" and "IMG_0001.MOV"
// for a live photo video "IMG_0001.MP4", or two sidecars with the same name in different takeouts
// for two different photos, the one whose taken time is closest to the date stored inside the
// media file wins. Without a readable date the first candidate is used
func pickSidecar(mediaPath string, matches []sidecarMatch) string {
	if len(matches) == 0 {
		return ""
	}

	bestRank := matches[0].rank
	for _, match := range matches[1:] {
		bestRank = min(bestRank, match.rank)
	}
	var best []string
	for _, match := range matches {
		if match.rank == bestRank {
			best = append(best, match.path)
		}
	}
	if len(best) == 1 {
		return best[0]
	}

	// Sidecars that describe the same photo, e.g. because the photo is part of several takeout
	// archives, are no ambiguity. Google's copies differ in details like view counts, so the
	// metadata that is written to the file is compared instead of the bytes
	if sameSidecarMetadata(best) {
		claimSidecar(best[0], mediaPath)
		return best[0]
	}

	fileDate, ok := ReadExifDate(mediaPath)
	if !ok {
		// Without a date to compare, at least do not reuse a sidecar that another file already took
		claimedSidecarsLock.Lock()
		defer claimedSidecarsLock.Unlock()
		for _, path := range best {
			if _, claimed := claimedSidecars[path]; !claimed {
				Log(LoggerWarn, "Several sidecars fit %s and the file has no date of its own, using %s. Please check this file", mediaPath, path)
				claimedSidecars[path] = mediaPath
				return path
			}
		}
		Log(LoggerWarn, "Several sidecars fit %s and all of them are also used by other files, using %s. Please check this file", mediaPath, best[0])
		return best[0]
	}

	chosen, chosenDistance := best[0], time.Duration(-1)
	for _, path := range best {
		metadata, err := ReadJsonMetadata(path)
		if err != nil {
			continue
		}
		taken, err := TakenTime(metadata)
		if err != nil {
			continue
		}
		// The file's own date has no reliable timezone, so compare on a scale of days.
		// Candidates are usually either the same moment or weeks apart
		distance := fileDate.Sub(taken).Abs()
		if chosenDistance < 0 || distance < chosenDistance {
			chosen, chosenDistance = path, distance
		}
	}
	if chosenDistance >= 0 {
		Log(LoggerInfo, "Several sidecars fit %s, using %s because its date is closest to the file's own date", mediaPath, chosen)
	}
	claimSidecar(chosen, mediaPath)
	return chosen
}

// The names a media file's sidecar may have, in order of preference: the file name as it is,
// because Google sometimes exports a sidecar of its own for an edited copy, then the parsed
// name without counter and edited suffix, and for edited copies and live photo videos with a
// counter the sidecar of the original photo, because Google does not always export a sidecar
// for those (same photo, same date)
func sidecarCandidates(fileName string) []sidecarCandidate {
	parts := splitName(fileName)
	media := parseMediaName(fileName)

	literal := mediaName{name: parts.lower, base: strings.TrimSuffix(parts.lower, parts.ext)}
	candidates := []sidecarCandidate{{name: literal}}
	if parts.edited && parts.trailing > 0 {
		// Google may have exported "IMG_0001-edited.jpg(1).json" for the second edited copy
		candidates = append(candidates, sidecarCandidate{name: mediaName{name: parts.stemNoCounter + parts.ext, base: parts.stemNoCounter, index: parts.trailing}})
	}
	if media != literal {
		candidates = append(candidates, sidecarCandidate{name: media})
	}
	if media.index > 0 && IsVideoFile(fileName) {
		// "IMG_0001(1).MP4" is usually the second live photo video of IMG_0001, not a video of IMG_0001(1).
		// That is a guess, so a match through this candidate is checked against the file's own metadata
		candidates = append(candidates, sidecarCandidate{name: mediaName{name: media.name, base: media.base}, guess: true})
	}

	return candidates
}

// A name a sidecar may have for a media file. guess marks the candidates that assume a relation
// Google's naming does not guarantee
type sidecarCandidate struct {
	name  mediaName
	guess bool
}

// Compares a sidecar found through a guess with what the media file says about itself and warns
// when they clearly describe different moments or places. The sidecar is still used, because the
// file's own metadata is often an export time or the location of a later re-upload
func warnIfSidecarDisagrees(mediaPath string, sidecarPath string) {
	metadata, err := ReadJsonMetadata(sidecarPath)
	if err != nil {
		return
	}
	if fileDate, ok := ReadExifDate(mediaPath); ok {
		if taken, err := TakenTime(metadata); err == nil {
			if days := fileDate.Sub(taken).Abs().Hours() / 24; days > 1 {
				Log(LoggerWarn, "%s uses the sidecar of its photo, but its own date (%s) is %.0f days from the sidecar's (%s). Please check this file", mediaPath, fileDate.Format("2006-01-02"), days, taken.Local().Format("2006-01-02"))
			}
		}
	}
	if geo := metadata.location(); geo.hasLocation() {
		if lat, lon, ok := readExistingLocation(mediaPath); ok {
			if km := distanceMetres(lat, lon, geo.Latitude, geo.Longitude) / 1000; km > 50 {
				Log(LoggerWarn, "%s uses the sidecar of its photo, but its own location is %.0f km from the sidecar's. Please check this file", mediaPath, km)
			}
		}
	}
}

// Find a matching sidecar JSON in the directory of the media file
func FindSidecar(imagePath string) (string, error) {
	return FindSidecarIn([]string{filepath.Dir(imagePath)}, imagePath)
}

// Find the sidecar JSON of a media file in the given directories, usually its own folder and the
// same folder in the other takeout archives. Exact names are searched in all directories before
// looser matches are accepted, so a sidecar in another takeout wins over the sidecar of a
// similarly named file in the same folder
func FindSidecarIn(dirs []string, mediaPath string) (string, error) {
	return FindSidecarInExcluding(dirs, mediaPath, nil)
}

// Like FindSidecarIn, but sidecars in the exclude set are ignored, e.g. because they were already
// moved away while organizing
func FindSidecarInExcluding(dirs []string, mediaPath string, exclude map[string]bool) (string, error) {
	candidates := sidecarCandidates(filepath.Base(mediaPath))

	for _, exactOnly := range []bool{true, false} {
		for _, candidate := range candidates {
			var matches []sidecarMatch
			for _, dir := range dirs {
				entries, err := ReadDirCached(dir)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return "", err
				}
				for _, match := range matchSidecars(dir, entries, candidate.name, exactOnly) {
					if !exclude[match.path] {
						matches = append(matches, match)
					}
				}
			}
			if path := pickSidecar(mediaPath, matches); path != "" {
				if candidate.guess {
					warnIfSidecarDisagrees(mediaPath, path)
				}
				return path, nil
			}
		}
	}

	return "", nil
}

// Detects the real type of a media file from its first bytes and returns the matching
// extension, or "" if the type is not one that Google is known to mislabel.
// Google sometimes exports JPEGs as .png or .heic, which exiftool refuses to write to
func DetectRealExtension(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	header := make([]byte, 16)
	n, _ := io.ReadFull(file, header)
	header = header[:n]
	if len(header) < 12 {
		return ""
	}

	switch {
	case bytes.HasPrefix(header, []byte{0xFF, 0xD8, 0xFF}):
		return ".jpg"
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(header, []byte("GIF8")):
		return ".gif"
	case bytes.HasPrefix(header, []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return ".webp"
	case bytes.Equal(header[4:8], []byte("ftyp")):
		brand := string(header[8:12])
		switch {
		case strings.HasPrefix(brand, "hei"), strings.HasPrefix(brand, "mif"), strings.HasPrefix(brand, "msf"):
			return ".heic"
		case brand == "qt  ":
			return ".mov"
		default:
			return ".mp4"
		}
	}
	return ""
}

// Returns the file name with the extension replaced by the real type of the file, if Google
// exported it with a wrong extension. Videos are left alone because .mp4 and .mov are
// interchangeable containers (see RestoreMOVExtension)
func CorrectedFileName(path string) string {
	fileName := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(fileName))
	real := DetectRealExtension(path)
	if real == "" || real == ext || (ext == ".jpeg" && real == ".jpg") || (ext == ".heif" && real == ".heic") || IsVideoFile(fileName) || IsVideoFile(real) {
		return fileName
	}
	return strings.TrimSuffix(fileName, filepath.Ext(fileName)) + real
}

// Checks if the file at the given path has the specified extension
func IsNameExtension(extension string, path string) bool {
	return strings.EqualFold(filepath.Ext(path), extension)
}

// Checks whether a directory is a standart google year folder
func IsYearFolder(dirPath string) (bool, error) {
	// Year folder prefixes of some countries
	// yearPrefixes is mostly made by AI. I have not verified these, but i assume they are primarily correct.
	// Please create an issue if you find any mistakes or if you want to add more languages.
	yearPrefixes := []string{
		"Photos from ",     // English
		"Fotos von ",       // German
		"Photos de ",       // French
		"Foto del ",        // Italian
		"Fotos de ",        // Spanish / Portuguese
		"Foto's van ",      // Dutch
		"Zdjęcia z ",       // Polish
		"Фотографии из ",   // Russian
		"Foton från ",      // Swedish
		"Bilder fra ",      // Norwegian
		"Billeder fra ",    // Danish
		"Fotoğraflar ",     // Turkish
		"Fotografie z ",    // Czech
		"Fotók a ",         // Hungarian
		"Φωτογραφίες από ", // Greek
		"Fotografii din ",  // Romanian
		"Foto dari ",       // Indonesian
		"รูปภาพจาก ",       // Thai
		"Ảnh từ ",          // Vietnamese
	}

	for _, prefix := range yearPrefixes {
		if strings.HasPrefix(dirPath, prefix) {
			// The rest of the string has to be 4 characters long
			yearPart := strings.TrimPrefix(dirPath, prefix)
			if matched, _ := regexp.MatchString(`^\d{4}$`, yearPart); matched {
				return true, nil
			}
		}
	}
	return false, nil
}

// Checks whether a file, that is provided using its path, is a media file
func IsMediaFile(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	isImage := slices.Contains(imageExtensions, extension)
	isVideo := slices.Contains(videoExtensions, extension)
	return isImage || isVideo
}

// Attempts to find an image file with the same base name as the video file
// This is used for live photos where the metadata is the images sidecar
// I think error handling could be improved here
func FindImagePartner(videoPath string) (string, error) {
	if !IsVideoFile(videoPath) {
		return "", nil
	}

	dir := filepath.Dir(videoPath)
	extension := filepath.Ext(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), extension)

	// Check all image extensions for a match
	for _, imgExt := range imageExtensions {
		candidate := filepath.Join(dir, base+imgExt)

		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}

		// Try uppercase extension
		candidateUpper := filepath.Join(dir, base+strings.ToUpper(imgExt))
		if _, err := os.Stat(candidateUpper); err == nil {
			return candidateUpper, nil
		}
	}

	return "", nil
}

// FindSourceRoots returns the folders that have the Google Photos folder structure (subfolders
// with media files). If the given path has that structure itself, only the path is returned.
// Otherwise up to two levels below are searched, to support selecting the "Takeout" folder or a
// folder containing multiple extracted takeout archives, e.g. "Takeout/Google Photos" and "Takeout 2/Google Photos"
func FindSourceRoots(path string) ([]string, error) {
	if dirHasMediaSubdirs(path) {
		return []string{path}, nil
	}

	subdirs, err := DiscoverDirs(path)
	if err != nil {
		return nil, err
	}

	var roots []string
	for _, sub := range subdirs {
		child := filepath.Join(path, sub.Name())
		if dirHasMediaSubdirs(child) {
			roots = append(roots, child)
			continue
		}

		grandchildren, _ := DiscoverDirs(child)
		for _, gc := range grandchildren {
			gcPath := filepath.Join(child, gc.Name())
			if dirHasMediaSubdirs(gcPath) {
				roots = append(roots, gcPath)
			}
		}
	}

	if len(roots) == 0 {
		return nil, fmt.Errorf("no media files found in folder structure")
	}
	return roots, nil
}

// Checks whether any direct subfolder of path contains a media file or a sidecar.
// The last takeout archive sometimes contains nothing but sidecars of files in the other archives
func dirHasMediaSubdirs(path string) bool {
	subdirs, err := DiscoverDirs(path)
	if err != nil {
		return false
	}
	for _, sub := range subdirs {
		files, err := os.ReadDir(filepath.Join(path, sub.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if IsMediaFile(f.Name()) {
				return true
			}
			// Only sidecars named after a media file count, Google also exports other JSON files
			if sidecarName, _, isSidecar := parseSidecarName(f.Name()); isSidecar && IsMediaFile(sidecarName) {
				return true
			}
		}
	}
	return false
}

// Counts the media files directly inside a folder
func CountMediaFiles(dir string) int {
	count := 0
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if !file.IsDir() && IsMediaFile(file.Name()) {
			count++
		}
	}
	return count
}

// Counts all processable files in all Google Photos folders of the source path
func CountProcessableFiles(sourcePath string) (int, error) {
	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		return 0, err
	}

	if !fileInfo.IsDir() {
		return 0, fmt.Errorf("source path is not a directory")
	}

	roots, err := FindSourceRoots(sourcePath)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, root := range roots {
		subdirs, err := DiscoverDirs(root)
		if err != nil {
			return 0, err
		}

		for _, dir := range subdirs {
			files, _ := os.ReadDir(filepath.Join(root, dir.Name()))
			for _, file := range files {
				if !file.IsDir() && IsMediaFile(file.Name()) {
					count++
				}
			}
		}
	}

	if count == 0 {
		return 0, fmt.Errorf("no media files found in folder structure")
	}
	return count, nil
}

// Detect the month of a file based on its sidecar metadata
// Returns the month as an integer between 1 and 12
// Falls back to the file's own metadata, a date in the file name and then to the modification time
// if there is no usable sidecar. The file's own metadata is only read if exiftool is running
func DetectFileMonth(sourcePath string, sidecarPath string) (int, error) {
	if sidecarPath != "" {
		metadata, err := ReadJsonMetadata(sidecarPath)
		if err == nil {
			var timestamp int64
			timestamp, err = strconv.ParseInt(metadata.PhotoTakenTime.Timestamp, 10, 64)
			if err == nil {
				// Use the timezone of the location, like the dates written into the file
				geo := metadata.location()
				loc := time.Local
				if geo.hasLocation() {
					loc = getPhotoTimezone(geo.Latitude, geo.Longitude)
				}
				return int(time.Unix(timestamp, 0).In(loc).Month()), nil
			}
		}
		Log(LoggerWarn, "Could not read date from sidecar %s, using file name or modification date instead: %v", sidecarPath, err)
	}

	if date, ok := ReadExifDate(sourcePath); ok {
		return int(date.Month()), nil
	}

	if date, ok := ParseDateFromFileName(filepath.Base(sourcePath)); ok {
		return int(date.Month()), nil
	}

	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		return 0, err
	}

	return int(fileInfo.ModTime().Month()), nil
}

// Matches dates like 20190704, 2019-07-04, 2019_07_04 or 2019.07.04 in file names,
// optionally followed by a time like 123456, 12-34-56 or 12.34.56
var fileNameDateRegex = regexp.MustCompile(
	`(?:^|[^0-9])(\d{4})[-_.]?(\d{2})[-_.]?(\d{2})(?:[-_. T]?(\d{2})[-_.]?(\d{2})[-_.]?(\d{2}))?(?:[^0-9]|$)`,
)

// Parses a date from a file name like "IMG_20190704_123456.jpg" or "Screenshot 2019-07-04 at 12.34.56.png".
// The date is interpreted as local time like the dates from sidecars
func ParseDateFromFileName(fileName string) (time.Time, bool) {
	name := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	match := fileNameDateRegex.FindStringSubmatch(name)
	if match == nil {
		return time.Time{}, false
	}

	year, _ := strconv.Atoi(match[1])
	month, _ := strconv.Atoi(match[2])
	day, _ := strconv.Atoi(match[3])
	hour, minute, second := 0, 0, 0
	if match[4] != "" {
		hour, _ = strconv.Atoi(match[4])
		minute, _ = strconv.Atoi(match[5])
		second, _ = strconv.Atoi(match[6])
	}

	if year < 1990 || year > time.Now().Year()+1 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}

	// time.Date normalizes invalid dates like February 30, so compare the result to reject those
	date := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.Local)
	if date.Month() != time.Month(month) || date.Day() != day {
		return time.Time{}, false
	}

	return date, true
}

func ResolveOutputDir(
	fixerCtx *FixerContext,
	sourcePath string,
	sidecarPath string,
	sourceDirName string,
	isYearFolder bool,
) (string, error) {
	if fixerCtx.Options.Flatten {
		return fixerCtx.OutputRoot, nil
	}

	targetDir := fixerCtx.OutputRoot
	if sourceDirName != "" /*&& !fixerCtx.Options.IgnoreAlbums && !isYearFolder*/ {
		targetDir = filepath.Join(targetDir, sourceDirName)
	}

	if !fixerCtx.Options.MonthSubfolders {
		return targetDir, nil
	}

	month, err := DetectFileMonth(sourcePath, sidecarPath)
	if err != nil {
		return "", err
	}

	return filepath.Join(targetDir, strconv.Itoa(month)), nil
}
