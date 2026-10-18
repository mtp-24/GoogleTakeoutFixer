package fixer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Writes files with the given contents below root
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Lists every file below root as "relative path = sha256", sorted
func treeDigest(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(filepath.Base(path), "GoogleTakeoutFixer-") {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		io.Copy(hash, file)
		rel, _ := filepath.Rel(root, path)
		lines = append(lines, fmt.Sprintf("%s = %x", rel, hash.Sum(nil)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return lines
}

func runOrganize(t *testing.T, source string, output string, options ProcessOptions) {
	t.Helper()
	t.Chdir(t.TempDir())
	progressCh := make(chan Progress)
	go func() {
		for range progressCh {
		}
	}()
	if err := Organize(context.Background(), source, output, progressCh, options); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizeAndUndo(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Takeout Parent Folder")
	output := filepath.Join(root, "Google Photos")
	sidecar := `{"title":"x","photoTakenTime":{"timestamp":"1592222400"}}`
	writeTree(t, source, map[string]string{
		// Same year folder split over two archives, sidecar in the other archive than its photo
		"Takeout 1/Google Photos/Photos from 2020/IMG_1.jpg":                            "photo 1",
		"Takeout 2/Google Photos/Photos from 2020/IMG_1.jpg.supplemental-metadata.json": sidecar,
		"Takeout 2/Google Photos/Photos from 2020/IMG_2.jpg":                            "photo 2",
		"Takeout 2/Google Photos/Photos from 2020/IMG_2.jpg.supplemental-metadata.json": sidecar,
		"Takeout 2/Google Photos/Photos from 2020/IMG_2-edited.jpg":                     "photo 2 edited",
		// Identical copy of IMG_2 in another archive -> duplicate
		"Takeout 3/Google Photos/Photos from 2020/IMG_2.jpg":                            "photo 2",
		"Takeout 3/Google Photos/Photos from 2020/IMG_2.jpg.supplemental-metadata.json": sidecar,
		// Different photo with the same name as IMG_1, with its live photo video -> both get "(1)"
		"Takeout 3/Google Photos/Photos from 2020/IMG_1.jpg":                            "another photo 1",
		"Takeout 3/Google Photos/Photos from 2020/IMG_1.jpg.supplemental-metadata.json": sidecar,
		"Takeout 3/Google Photos/Photos from 2020/IMG_1.MP4":                            "live video of another photo 1",
		// Album with its metadata file in both archives, and a sidecar nobody owns
		"Takeout 1/Google Photos/Holiday/metadata.json":                        `{"title":"Holiday"}`,
		"Takeout 1/Google Photos/Holiday/IMG_1.jpg":                            "photo 1",
		"Takeout 3/Google Photos/Holiday/metadata.json":                        `{"title":"Holiday"}`,
		"Takeout 3/Google Photos/Holiday/IMG_9.jpg.supplemental-metadata.json": sidecar,
		"Takeout 3/Google Photos/Holiday/notes.txt":                            "not a photo",
		// A JPEG exported as .png
		"Takeout 1/Google Photos/Photos from 2021/shot.png":                            "\xff\xd8\xff\xe0 jpeg bytes........",
		"Takeout 1/Google Photos/Photos from 2021/shot.png.supplemental-metadata.json": sidecar,
	})
	before := treeDigest(t, source)

	runOrganize(t, source, output, ProcessOptions{})

	got := treeDigest(t, output)
	want := []string{
		"Holiday/IMG_1.jpg",
		"Holiday/metadata.json",
		"Photos from 2020/IMG_1(1).MP4",
		"Photos from 2020/IMG_1(1).jpg",
		"Photos from 2020/IMG_1(1).jpg.supplemental-metadata.json",
		"Photos from 2020/IMG_1.jpg",
		"Photos from 2020/IMG_1.jpg.supplemental-metadata.json",
		"Photos from 2020/IMG_2-edited.jpg",
		"Photos from 2020/IMG_2.jpg",
		"Photos from 2020/IMG_2.jpg.supplemental-metadata.json",
		"Photos from 2021/shot.jpg",
		"Photos from 2021/shot.jpg.supplemental-metadata.json",
		"_duplicates/Holiday/metadata.json",
		"_duplicates/Photos from 2020/IMG_2.jpg",
		"_duplicates/Photos from 2020/IMG_2.jpg.supplemental-metadata.json",
		"_unmatched sidecars/Holiday/IMG_9.jpg.supplemental-metadata.json",
		"_unsorted/Holiday/notes.txt",
	}
	var gotPaths []string
	for _, line := range got {
		gotPaths = append(gotPaths, strings.SplitN(line, " = ", 2)[0])
	}
	if strings.Join(gotPaths, "\n") != strings.Join(want, "\n") {
		t.Errorf("organized tree:\n%s\nwant:\n%s", strings.Join(gotPaths, "\n"), strings.Join(want, "\n"))
	}

	// The renamed photo must be the other one, and content is never changed
	if content, _ := os.ReadFile(filepath.Join(output, "Photos from 2020", "IMG_1(1).jpg")); string(content) != "another photo 1" {
		t.Errorf("IMG_1(1).jpg has content %q", content)
	}
	if remaining := treeDigest(t, source); len(remaining) != 0 {
		t.Errorf("files left in the source: %v", remaining)
	}
	for _, takeout := range []string{"Takeout 1", "Takeout 2", "Takeout 3"} {
		if _, err := os.Stat(filepath.Join(source, takeout)); !os.IsNotExist(err) {
			t.Errorf("empty folder %s was not removed", takeout)
		}
	}

	// Undo brings back the exact original tree
	journals, _ := filepath.Glob(filepath.Join(output, "GoogleTakeoutFixer-organize-*.jsonl"))
	if len(journals) != 1 {
		t.Fatalf("expected one journal, found %v", journals)
	}
	if err := Undo(journals[0]); err != nil {
		t.Fatal(err)
	}
	if after := treeDigest(t, source); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("undo did not restore the source:\n%s\nwant:\n%s", strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
	if leftovers := treeDigest(t, output); len(leftovers) != 0 {
		t.Errorf("files left in the output after undo: %v", leftovers)
	}
}

func TestOrganizeDryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "in")
	output := filepath.Join(root, "out")
	writeTree(t, source, map[string]string{
		"Takeout 1/Google Photos/Photos from 2020/IMG_1.jpg":      "photo 1",
		"Takeout 1/Google Photos/Photos from 2020/IMG_1.jpg.json": `{"photoTakenTime":{"timestamp":"1592222400"}}`,
	})
	before := treeDigest(t, source)

	runOrganize(t, source, output, ProcessOptions{DryRun: true})

	if after := treeDigest(t, source); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("dry run changed the source")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("dry run created the output folder")
	}
}

func TestOwnsSidecar(t *testing.T) {
	for _, test := range []struct {
		media, sidecar string
		owner          bool
	}{
		{"000008-edited(1).JPG", "000008.JPG.supplemental-metadata(1).json", false},
		{"000008-edited(1).JPG", "000008-edited.JPG.supplemental-metadata(1).json", true},
		{"000008(1).JPG", "000008.JPG.supplemental-metadata(1).json", true},
		{"000008(1)-edited.JPG", "000008(1)-edited.JPG.supplemental-metadata.json", true},
		{"IMG_1.MP4", "IMG_1.HEIC.supplemental-metadata.json", false},
	} {
		if got := ownsSidecar(test.media, test.sidecar); got != test.owner {
			t.Errorf("ownsSidecar(%q, %q) = %v; want %v", test.media, test.sidecar, got, test.owner)
		}
	}
}
