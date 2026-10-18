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
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/bradfitz/latlong"
)

// Struct to hold the structure of the JSON metadata
type imageMetadata struct {
	Title          string `json:"title"`
	Description    string `json:"description"`
	PhotoTakenTime struct {
		Timestamp string `json:"timestamp"`
		Formatted string `json:"formatted"`
	} `json:"photoTakenTime"`
	GeoData     geoData `json:"geoData"`
	GeoDataExif geoData `json:"geoDataExif"`
	People      []struct {
		Name string `json:"name"`
	} `json:"people"`
	Favorited      bool `json:"favorited"`
	Archived       bool `json:"archived"`
	Trashed        bool `json:"trashed"`
	InLockedFolder bool `json:"inLockedFolder"`
}

type geoData struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  float64 `json:"altitude"`
}

// Google's own location, or the location from the original file's metadata if Google has none
func (meta imageMetadata) location() geoData {
	if meta.GeoData.Latitude == 0 && meta.GeoData.Longitude == 0 {
		return meta.GeoDataExif
	}
	return meta.GeoData
}

func (geo geoData) hasLocation() bool {
	return geo.Latitude != 0 || geo.Longitude != 0
}

// Exiftool reads argument values with -E, so text has to be HTML escaped. Most importantly
// a line break inside a description would otherwise end the argument (issue #26)
func escapeExifValue(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#13;", "\n", "&#10;")
	return replacer.Replace(s)
}

// Google stores the original file name as title. Writing that into the file would show
// up as a caption in most photo apps, so the title is only written if it is a real title
func isRealTitle(title string, fileName string) bool {
	if title == "" {
		return false
	}
	stem := func(name string) string {
		media := parseMediaName(name)
		return media.base
	}
	return stem(title) != stem(fileName)
}

// Existing dates of a file, read before new ones are written
type existingDate struct {
	wallClock time.Time // Local time as written in the file, in the UTC location
	offsetSec int
	hasOffset bool
	found     bool
}

// Reads the date and timezone offset a file already has. Photos store a local time and
// optionally an offset, videos may have Keys:CreationDate with an offset (Apple)
func readExistingDate(filePath string) existingDate {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return existingDate{}
	}

	// Raw values: exiftool's date formatting would fill in this computer's timezone where the file has none
	lines, err := runExifTool([]string{
		"-DateTimeOriginal", "-OffsetTimeOriginal", "-Keys:CreationDate", "-s3", "-f",
		"-charset", "filename=utf8", filePath,
	})
	if err != nil || len(lines) < 3 {
		return existingDate{}
	}

	result := existingDate{}
	parse := func(value string) bool {
		value = strings.TrimSpace(value)
		if value == "-" || value == "" {
			return false
		}
		if t, err := time.Parse("2006:01:02 15:04:05-07:00", value); err == nil && t.Year() >= 1990 {
			_, offset := t.Zone()
			result = existingDate{wallClock: time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC), offsetSec: offset, hasOffset: true, found: true}
			return true
		}
		if t, err := time.Parse("2006:01:02 15:04:05", value); err == nil && t.Year() >= 1990 {
			result = existingDate{wallClock: t, found: true}
			return true
		}
		return false
	}

	// Photo date with its offset, if there is one
	if parse(lines[0]) && !result.hasOffset {
		if offset := strings.TrimSpace(lines[1]); offset != "-" && offset != "" {
			if t, err := time.Parse("-07:00", offset); err == nil {
				_, result.offsetSec = t.Zone()
				result.hasOffset = true
			}
		}
	}
	if !result.found {
		parse(lines[2])
	}
	return result
}

// Decides which timezone the taken time is written in. Order of preference:
// the timezone at the photo's location, the offset already stored in the file, the
// local time already stored in the file, and finally the timezone of this computer
func photoLocation(utcTime time.Time, geo geoData, existing existingDate) (*time.Location, string) {
	if geo.hasLocation() {
		return getPhotoTimezone(geo.Latitude, geo.Longitude), "location"
	}
	if existing.hasOffset {
		return time.FixedZone("Photo", existing.offsetSec), "offset in file"
	}
	if existing.found {
		// The file knows the local time but not the offset. Keep that local time as long as it is a
		// plausible offset from the taken time, since cameras count in whole or half hours
		diff := existing.wallClock.Sub(time.Date(utcTime.Year(), utcTime.Month(), utcTime.Day(), utcTime.Hour(), utcTime.Minute(), utcTime.Second(), 0, time.UTC))
		if diff >= -14*time.Hour && diff <= 14*time.Hour && diff%(15*time.Minute) == 0 {
			return time.FixedZone("Photo", int(diff/time.Second)), "local time in file"
		}
	}
	return time.Local, "this computer"
}

// Reads JSON and returns some of its metadata contents using the imageMetadata struct
func ReadJsonMetadata(jsonPath string) (imageMetadata, error) {
	var data imageMetadata

	jsonFile, err := os.Open(jsonPath)
	if err != nil {
		return data, err
	}
	defer jsonFile.Close()

	byteValue, err := io.ReadAll(jsonFile)
	if err != nil {
		return data, err
	}

	return data, json.Unmarshal(byteValue, &data)
}

// Checks that a sidecar can be read and contains a valid timestamp, without applying it
func CheckJsonMetadata(jsonPath string) error {
	metadata, err := ReadJsonMetadata(jsonPath)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseInt(metadata.PhotoTakenTime.Timestamp, 10, 64); err != nil {
		return fmt.Errorf("invalid timestamp: %v", err)
	}
	return nil
}

// Helper to find exiftool (bundled or in PATH)
func getExifToolPath() string {
	exePath, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exePath)
		exifName := "exiftool"
		if runtime.GOOS == "windows" {
			exifName = "exiftool.exe"
		}
		bundledPath := filepath.Join(dir, exifName)
		if _, err := os.Stat(bundledPath); err == nil {
			return bundledPath
		}
	}

	// Apps started from Finder do not have the Homebrew folders in their PATH
	if _, err := exec.LookPath("exiftool"); err != nil && runtime.GOOS == "darwin" {
		for _, brewPath := range []string{"/opt/homebrew/bin/exiftool", "/usr/local/bin/exiftool"} {
			if _, err := os.Stat(brewPath); err == nil {
				return brewPath
			}
		}
	}
	return "exiftool"
}

// Start a persistent exiftool process
func InitializeExifTool() error {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd != nil {
		// Already initialized
		return nil
	}

	exifToolCmd = exec.Command(getExifToolPath(), "-stay_open", "True", "-@", "-")

	var err error = nil
	exifToolStdin, err = exifToolCmd.StdinPipe()
	if err != nil {
		return err
	}

	// Exiftool prints errors like "Error: Not a valid GIF" to stderr, so read stderr and stdout
	// together. Otherwise failed writes would go unnoticed (issue #26)
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	exifToolCmd.Stdout = outputWriter
	exifToolCmd.Stderr = outputWriter
	exifToolStdout = outputReader
	exifToolScanner = bufio.NewScanner(exifToolStdout)

	err = exifToolCmd.Start()
	// The child process has its own copy of the writer
	outputWriter.Close()
	if err != nil {
		outputReader.Close()
		exifToolCmd = nil
		return err
	}

	return nil
}

// Close the persistent exiftool process
func CloseExifTool() {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd != nil {
		exifToolStdin.Write([]byte("-stay_open\nFalse\n"))
		exifToolCmd.Wait()
		exifToolStdout.Close()
		exifToolCmd = nil
	}
}

// The time a photo was taken according to its sidecar
func TakenTime(meta imageMetadata) (time.Time, error) {
	timestampInt, err := strconv.ParseInt(meta.PhotoTakenTime.Timestamp, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp: %v", err)
	}
	return time.Unix(timestampInt, 0).UTC(), nil
}

// Builds the exiftool arguments that write the sidecar metadata into the file at filePath.
// originalName is the file name Google exported, used to decide whether the title is a real one
func metadataArgs(filePath string, originalName string, meta imageMetadata) ([]string, time.Time, error) {
	isVideo := IsVideoFile(filePath)
	geo := meta.location()

	// -E makes exiftool decode HTML entities, which is how line breaks in descriptions survive the argument file
	args := []string{
		"-overwrite_original",
		"-E",
		"-charset", "filename=utf8",
	}

	utcTime, err := TakenTime(meta)
	if err != nil {
		// A sidecar without a usable taken time: keep the date the file has, or take it from the
		// file name, and still write the rest of the metadata
		Log(LoggerWarn, "Sidecar of %s has no usable taken time (%v), using the date in the file or its name instead", originalName, err)
		if date, ok := ReadExifDate(filePath); ok {
			utcTime = date.UTC()
		} else if date, ok := ParseDateFromFileName(originalName); ok {
			utcTime = date.UTC()
		}
	}

	if !utcTime.IsZero() {
		photoLoc, tzSource := photoLocation(utcTime, geo, readExistingDate(filePath))
		localTime := utcTime.In(photoLoc)
		_, offsetSec := localTime.Zone()
		offsetStr := formatTimezoneOffset(offsetSec)
		exifTimeWithTZ := localTime.Format("2006:01:02 15:04:05") + offsetStr
		if tzSource == "this computer" {
			Log(LoggerInfo, "No timezone known for %s, writing the taken time in this computer's timezone (%s)", originalName, offsetStr)
		}

		args = append(args,
			"-AllDates="+exifTimeWithTZ,
			"-FileCreateDate="+exifTimeWithTZ,
			"-FileModifyDate="+exifTimeWithTZ,
		)
		if isVideo {
			// QuickTime stores dates in UTC. With QuickTimeUTC exiftool converts the local time with offset
			// to UTC instead of storing the local time as if it was UTC. Keys:CreationDate keeps the offset
			args = append(args,
				"-api", "QuickTimeUTC",
				"-TrackCreateDate="+exifTimeWithTZ,
				"-MediaCreateDate="+exifTimeWithTZ,
				"-Keys:CreationDate="+exifTimeWithTZ,
			)
		} else if tzSource != "local time in file" {
			// When only the camera's clock is known, the offset is not. Google assumed UTC for such
			// photos, which is a default and not knowledge, so no offset is written in that case
			args = append(args,
				"-OffsetTime="+offsetStr,
				"-OffsetTimeOriginal="+offsetStr,
				"-OffsetTimeDigitized="+offsetStr,
			)
		}
	}

	if isRealTitle(meta.Title, originalName) {
		args = append(args, "-Title="+escapeExifValue(meta.Title))
	}

	if meta.Description != "" {
		description := escapeExifValue(meta.Description)
		args = append(args, "-XMP-dc:Description="+description)
		if isVideo {
			args = append(args, "-Keys:Description="+description)
		} else {
			args = append(args, "-ImageDescription="+description, "-Caption-Abstract="+description)
		}
	}

	// People tagged in Google Photos, as keywords and as the standard "person in image" field.
	// Added to existing keywords instead of replacing them. Removing the name first is exiftool's
	// way of not adding it twice when a run is repeated
	for _, person := range meta.People {
		if name := escapeExifValue(person.Name); person.Name != "" {
			args = append(args,
				"-XMP-iptcExt:PersonInImage-="+name, "-XMP-iptcExt:PersonInImage+="+name,
				"-XMP-dc:Subject-="+name, "-XMP-dc:Subject+="+name,
			)
		}
	}

	if meta.Favorited {
		args = append(args, "-Rating=5")
	}

	// EXIF uses N E S W for geodata
	if geo.hasLocation() {
		lat, lon := geo.Latitude, geo.Longitude

		latRef, lonRef := "N", "E"
		if lat < 0 {
			latRef = "S"
		}
		if lon < 0 {
			lonRef = "W"
		}
		altitudeRef := "Above Sea Level"
		if geo.Altitude < 0 {
			altitudeRef = "Below Sea Level"
		}

		args = append(args,
			fmt.Sprintf("-GPSLatitude=%f", math.Abs(lat)),
			fmt.Sprintf("-GPSLatitudeRef=%s", latRef),
			fmt.Sprintf("-GPSLongitude=%f", math.Abs(lon)),
			fmt.Sprintf("-GPSLongitudeRef=%s", lonRef),
			fmt.Sprintf("-GPSAltitude=%f", math.Abs(geo.Altitude)),
			fmt.Sprintf("-GPSAltitudeRef=%s", altitudeRef),
		)
		if isVideo {
			// Video players and photo apps read the location of a video from the QuickTime keys
			coordinates := fmt.Sprintf("%f, %f, %f", lat, lon, geo.Altitude)
			args = append(args, "-Keys:GPSCoordinates="+coordinates, "-UserData:GPSCoordinates="+coordinates)
		}
	}

	return append(args, filePath), utcTime, nil
}

// Apply all available metadata to a file
func ApplyMetadata(filePath string, originalName string, meta imageMetadata) error {
	args, utcTime, err := metadataArgs(filePath, originalName, meta)
	if err != nil {
		return err
	}

	// Use the persistent exiftool instance
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return fmt.Errorf("Exiftool is not initialized")
	}

	if _, err := runExifTool(args); err != nil {
		return err
	}

	// Set the file system modification time to match
	if !utcTime.IsZero() {
		if err := os.Chtimes(filePath, utcTime, utcTime); err != nil {
			return fmt.Errorf("failed to set file timestamps: %v", err)
		}
	}

	return nil
}

// Runs a command with the persistent exiftool instance and returns its output lines.
// The exiftool mutex has to be locked by the caller
func runExifTool(args []string) ([]string, error) {
	command := strings.Join(args, "\n") + "\n-execute\n"
	if _, err := exifToolStdin.Write([]byte(command)); err != nil {
		return nil, fmt.Errorf("Failed to write to exiftool: %v", err)
	}

	var lines []string
	var exifErr error
	for exifToolScanner.Scan() {
		line := exifToolScanner.Text()
		if line == "{ready}" {
			break
		}
		// Exiftool reports problems as "Error: ..." lines and in the final count. Tag values and
		// JSON output may contain the word too (e.g. GPSHPositioningError), so only whole lines count
		trimmed := strings.TrimSpace(line)
		if exifErr == nil && (strings.HasPrefix(trimmed, "Error") || strings.Contains(trimmed, "weren't updated")) {
			exifErr = fmt.Errorf("Exiftool error: %s", trimmed)
		}
		lines = append(lines, line)
	}

	if exifErr != nil {
		return lines, exifErr
	}

	if err := exifToolScanner.Err(); err != nil {
		return lines, fmt.Errorf("Failed to read from exiftool: %v", err)
	}

	return lines, nil
}

// Reads the date a photo or video was taken from its own metadata, if it has one
func ReadExifDate(filePath string) (time.Time, bool) {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return time.Time{}, false
	}

	// QuickTimeUTC converts video dates, which are stored in UTC, to local time like photo dates
	const layout = "2006:01:02 15:04:05"
	lines, err := runExifTool([]string{
		"-DateTimeOriginal", "-CreateDate", "-s3", "-api", "QuickTimeUTC", "-d", "%Y:%m:%d %H:%M:%S",
		"-charset", "filename=utf8", filePath,
	})
	if err != nil {
		return time.Time{}, false
	}

	for _, line := range lines {
		date, err := time.ParseInLocation(layout, strings.TrimSpace(line), time.Local)
		// Cameras without a set clock write dates like 0000:00:00 00:00:00
		if err == nil && date.Year() >= 1990 {
			return date, true
		}
	}

	return time.Time{}, false
}

// Sets the creation and modification date of a file, without changing its metadata
func SetFileDates(filePath string, date time.Time) error {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return fmt.Errorf("Exiftool is not initialized")
	}

	fileDate := date.Format("2006:01:02 15:04:05-07:00")
	_, err := runExifTool([]string{
		"-overwrite_original", "-FileCreateDate=" + fileDate, "-FileModifyDate=" + fileDate,
		"-charset", "filename=utf8", filePath,
	})
	return err
}

// GetMajorBrand reads the MajorBrand tag from a file using the persistent exiftool instance
func GetMajorBrand(filePath string) (string, error) {
	exifToolMutex.Lock()
	defer exifToolMutex.Unlock()

	if exifToolCmd == nil {
		return "", fmt.Errorf("exiftool not initialized")
	}

	if _, err := fmt.Fprintf(exifToolStdin, "-MajorBrand\n-s3\n-charset\nfilename=utf8\n%s\n-execute\n", filePath); err != nil {
		return "", err
	}

	var majorBrand string
	for exifToolScanner.Scan() {
		if line := exifToolScanner.Text(); line == "{ready}" {
			break
		} else if majorBrand == "" && !strings.Contains(line, "Error") && !strings.Contains(line, "Warning") {
			majorBrand = line
		}
	}

	return strings.TrimSpace(majorBrand), exifToolScanner.Err()
}

// Determine the timezone at a photo's GPS location using the "latlog" library
// If no GPS data is available, fall back to local time
func getPhotoTimezone(lat, lon float64) *time.Location {
	if lat == 0 && lon == 0 {
		return time.Local
	}

	tzName := latlong.LookupZoneName(lat, lon)
	if tzName == "" {
		// Fallback in case latlog fails to find a timezone
		Log(LoggerWarn, "Could not look up timezone for coordinates lat=%f, lon=%f", lat, lon)
		offsetSec := int(math.Round(lon/15.0)) * 3600
		return time.FixedZone("Photo", offsetSec)
	}

	loc, err := time.LoadLocation(tzName)
	if err != nil {
		// Fallback in case loading timezone fails
		Log(LoggerWarn, "Could not load timezone '%s'", tzName)
		offsetSec := int(math.Round(lon/15.0)) * 3600
		return time.FixedZone("Photo", offsetSec)
	}
	return loc
}

// Format a timezone offset in seconds as "+hh:mm" / "-hh:mm" for EXIF
// for example 3600 seconds becomes "+01:00"
func formatTimezoneOffset(offsetSec int) string {
	sign := "+"
	if offsetSec < 0 {
		sign = "-"
		offsetSec = -offsetSec
	}
	hours := offsetSec / 3600
	minutes := (offsetSec % 3600) / 60
	return fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}

// Exiftool process variables
var (
	exifToolCmd     *exec.Cmd
	exifToolStdin   io.WriteCloser
	exifToolStdout  io.ReadCloser
	exifToolScanner *bufio.Scanner
	exifToolMutex   sync.Mutex
)
