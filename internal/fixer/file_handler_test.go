package fixer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSidecarName(t *testing.T) {
	tests := []struct {
		fileName string
		name     string
		index    int
	}{
		{"IMG_0001.jpg.json", "img_0001.jpg", 0},
		{"IMG_0001.jpg.supplemental-metadata.json", "img_0001.jpg", 0},
		{"IMG_0001.jpg.supplemental-me.json", "img_0001.jpg", 0},
		{"IMG_0001.jpg.s.json", "img_0001.jpg", 0},
		{"IMG_0001.jpg(1).json", "img_0001.jpg", 1},
		{"IMG_0001.jpg.supplemental-metadata(2).json", "img_0001.jpg", 2},
		{"IMG_0001(3).jpg.json", "img_0001.jpg", 3},
		{"IMG_0001.json", "img_0001", 0},
	}

	for _, test := range tests {
		name, index, ok := parseSidecarName(test.fileName)
		if !ok || name != test.name || index != test.index {
			t.Errorf("parseSidecarName(%q) = %q, %d, %v; want %q, %d", test.fileName, name, index, ok, test.name, test.index)
		}
	}

	if _, _, ok := parseSidecarName("IMG_0001.jpg"); ok {
		t.Errorf("parseSidecarName accepted a file that is not a JSON file")
	}
}

func TestParseMediaName(t *testing.T) {
	tests := []struct {
		fileName string
		want     mediaName
	}{
		{"IMG_0001.JPG", mediaName{name: "img_0001.jpg", base: "img_0001"}},
		{"DSC_0123(2).jpg", mediaName{name: "dsc_0123.jpg", base: "dsc_0123", index: 2}},
		{"IMG_0001-edited.jpg", mediaName{name: "img_0001.jpg", base: "img_0001"}},
		{"IMG_0001-edited(1).jpg", mediaName{name: "img_0001.jpg", base: "img_0001"}}, // second edited copy of IMG_0001
		{"IMG_0001(1)-edited.jpg", mediaName{name: "img_0001.jpg", base: "img_0001", index: 1}},
		{"IMG_0001-bearbeitet.jpg", mediaName{name: "img_0001.jpg", base: "img_0001"}},
		{"IMG_5296-EFFECTS-edited.jpg", mediaName{name: "img_5296.jpg", base: "img_5296"}},
	}

	for _, test := range tests {
		if got := parseMediaName(test.fileName); got != test.want {
			t.Errorf("parseMediaName(%q) = %+v; want %+v", test.fileName, got, test.want)
		}
	}
}

// Creates empty files in a temporary directory and returns the directory
func createFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(ClearCache)
	return dir
}

func TestFindSidecar(t *testing.T) {
	dir := createFiles(t,
		// Duplicate names (issue #28)
		"DSC_0123.jpg", "DSC_0123(1).jpg", "DSC_0123(2).jpg",
		"DSC_0123.jpg.supplemental-metadata.json",
		"DSC_0123.jpg.supplemental-metadata(1).json",
		"DSC_0123.jpg.supplemental-metadata(2).json",
		// A name that is a prefix of another name must not take the other sidecar
		"IMG_1.jpg", "IMG_10.jpg", "IMG_10.jpg.json",
		// Edited copies use the original sidecar
		"IMG_2.jpg", "IMG_2-edited.jpg", "IMG_2.jpg.supplemental-meta.json",
		// Live photo video without its own sidecar
		"IMG_3.HEIC", "IMG_3.MP4", "IMG_3.HEIC.json",
		// Older naming without extension
		"IMG_4.png", "IMG_4.json",
		// Shortened sidecar name of a long file name
		"PXL_20230615_101010123.PORTRAIT.ORIGINAL_EXPORT.jpg",
		"PXL_20230615_101010123.PORTRAIT.ORIGINAL_EXPO.json",
		// "(1)" that is part of the original name
		"Screenshot (1).png", "Screenshot (1).png.json",
	)

	tests := []struct {
		media   string
		sidecar string
	}{
		{"DSC_0123.jpg", "DSC_0123.jpg.supplemental-metadata.json"},
		{"DSC_0123(1).jpg", "DSC_0123.jpg.supplemental-metadata(1).json"},
		{"DSC_0123(2).jpg", "DSC_0123.jpg.supplemental-metadata(2).json"},
		{"IMG_1.jpg", ""},
		{"IMG_10.jpg", "IMG_10.jpg.json"},
		{"IMG_2-edited.jpg", "IMG_2.jpg.supplemental-meta.json"},
		{"IMG_3.MP4", "IMG_3.HEIC.json"},
		{"IMG_4.png", "IMG_4.json"},
		{"PXL_20230615_101010123.PORTRAIT.ORIGINAL_EXPORT.jpg", "PXL_20230615_101010123.PORTRAIT.ORIGINAL_EXPO.json"},
		{"Screenshot (1).png", "Screenshot (1).png.json"},
	}

	for _, test := range tests {
		got, err := FindSidecar(filepath.Join(dir, test.media))
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if test.sidecar != "" {
			want = filepath.Join(dir, test.sidecar)
		}
		if got != want {
			t.Errorf("FindSidecar(%q) = %q; want %q", test.media, filepath.Base(got), test.sidecar)
		}
	}
}

func TestClaimDestPath(t *testing.T) {
	dir := createFiles(t, "existing.jpg")
	src := t.TempDir()
	source := func(name string, content string) string {
		path := filepath.Join(src, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	photoA := source("a.jpg", "photo a")
	photoB := source("b.jpg", "photo b")
	photoC := source("c.jpg", "photo c")
	copyOfA := source("a copy.jpg", "photo a")
	copyOfB := source("b copy.jpg", "photo b")
	fixerCtx := &FixerContext{}

	if _, existed, _ := fixerCtx.ClaimDestPath(filepath.Join(dir, "existing.jpg"), photoA); !existed {
		t.Errorf("file that existed before the run was not reported as existing")
	}

	first, existed, dup := fixerCtx.ClaimDestPath(filepath.Join(dir, "IMG_0001.jpg"), photoA)
	if existed || dup != "" || filepath.Base(first) != "IMG_0001.jpg" {
		t.Errorf("first claim = %q, %v, %q; want IMG_0001.jpg", filepath.Base(first), existed, dup)
	}

	// Different file with the same name from another source folder, e.g. when flattening
	second, existed, dup := fixerCtx.ClaimDestPath(filepath.Join(dir, "IMG_0001.jpg"), photoB)
	if existed || dup != "" || filepath.Base(second) != "IMG_0001(1).jpg" {
		t.Errorf("second claim = %q, %v, %q; want IMG_0001(1).jpg", filepath.Base(second), existed, dup)
	}

	// Case insensitive file systems treat these names as the same file
	third, _, _ := fixerCtx.ClaimDestPath(filepath.Join(dir, "img_0001.JPG"), photoC)
	if filepath.Base(third) != "img_0001(2).JPG" {
		t.Errorf("third claim = %q; want img_0001(2).JPG", filepath.Base(third))
	}

	// Identical copies, e.g. from another takeout archive, are reported as duplicates
	if _, _, dup := fixerCtx.ClaimDestPath(filepath.Join(dir, "IMG_0001.jpg"), copyOfA); dup != photoA {
		t.Errorf("identical copy of the first file: duplicateOf = %q; want %q", dup, photoA)
	}
	if _, _, dup := fixerCtx.ClaimDestPath(filepath.Join(dir, "IMG_0001.jpg"), copyOfB); dup != photoB {
		t.Errorf("identical copy of the renamed file: duplicateOf = %q; want %q", dup, photoB)
	}
}

func TestParseDateFromFileName(t *testing.T) {
	tests := []struct {
		fileName string
		want     string // empty if no date should be found
	}{
		{"IMG_20190704_123456.jpg", "2019-07-04 12:34:56"},
		{"IMG-20190704-WA0001.jpg", "2019-07-04 00:00:00"},
		{"Screenshot_2019-07-04-12-34-56.png", "2019-07-04 12:34:56"},
		{"2007-07-21 14.04.AVI", "2007-07-21 00:00:00"},
		{"PXL_20230615_101010123.jpg", "2023-06-15 00:00:00"},
		{"VID_2019.07.04_12.34.56.mp4", "2019-07-04 12:34:56"},
		{"IMG_0001.jpg", ""},
		{"FB_IMG_1592345678901.jpg", ""}, // Unix timestamp in milliseconds
		{"IMG_20190230_000000.jpg", ""},  // February 30
		{"DSC_123456789.jpg", ""},
	}

	for _, test := range tests {
		date, ok := ParseDateFromFileName(test.fileName)
		got := ""
		if ok {
			got = date.Format("2006-01-02 15:04:05")
		}
		if got != test.want {
			t.Errorf("ParseDateFromFileName(%q) = %q; want %q", test.fileName, got, test.want)
		}
	}
}

// Creates empty files at the given paths relative to a temporary directory and returns the directory
func createTree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range paths {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(ClearCache)
	return root
}

func TestFindSourceRoots(t *testing.T) {
	// Google Photos folder selected directly
	root := createTree(t, "Photos from 2020/IMG_1.jpg")
	if roots, err := FindSourceRoots(root); err != nil || len(roots) != 1 || roots[0] != root {
		t.Errorf("FindSourceRoots(Google Photos folder) = %v, %v; want [%s]", roots, err, root)
	}

	// Folder with multiple extracted takeouts
	root = createTree(t,
		"Takeout/Google Photos/Photos from 2020/IMG_1.jpg",
		"Takeout 2/Google Photos/Photos from 2020/IMG_2.jpg",
		"Takeout 2/Google Photos/Photos from 2020/IMG_2.jpg.json",
	)
	roots, err := FindSourceRoots(root)
	want := []string{filepath.Join(root, "Takeout", "Google Photos"), filepath.Join(root, "Takeout 2", "Google Photos")}
	if err != nil || len(roots) != 2 || roots[0] != want[0] || roots[1] != want[1] {
		t.Errorf("FindSourceRoots(multiple takeouts) = %v, %v; want %v", roots, err, want)
	}

	// Sidecar that ended up in another takeout
	fixerCtx := &FixerContext{SourceRoots: roots}
	got, err := fixerCtx.FindSidecar(filepath.Join(roots[0], "Photos from 2020", "IMG_2.jpg"))
	if err != nil || got != filepath.Join(roots[1], "Photos from 2020", "IMG_2.jpg.json") {
		t.Errorf("FindSidecar across takeouts = %q, %v", got, err)
	}

	// The last archive of a takeout may hold nothing but sidecars
	root = createTree(t,
		"Takeout 1/Google Photos/Photos from 2020/IMG_1.jpg",
		"Takeout 2/Google Photos/Photos from 2020/IMG_1.jpg.supplemental-metadata.json",
	)
	if roots, err := FindSourceRoots(root); err != nil || len(roots) != 2 {
		t.Errorf("FindSourceRoots(sidecar-only takeout) = %v, %v; want both takeouts", roots, err)
	}

	if _, err := FindSourceRoots(createTree(t, "notes.txt")); err == nil {
		t.Errorf("FindSourceRoots found media in a folder without media files")
	}
}

func TestSidecarFallbacks(t *testing.T) {
	dir := createFiles(t,
		// Second edited copy of a photo: Google exports no sidecar for it, the original's sidecar is used
		"20160918_071839.jpg", "20160918_071839.jpg.supplemental-metadata.json",
		"20160918_071839-edited.jpg", "20160918_071839-edited(1).jpg",
		// Live photo video with a counter but without its own sidecar or partner image
		"IMG_0248.HEIC", "IMG_0248.HEIC.supplemental-metadata.json", "IMG_0248(1).MP4",
		// A numbered photo must NOT fall back to the sidecar of the unnumbered photo, that is a different photo
		"DSC_0001.jpg", "DSC_0001.jpg.json", "DSC_0001(1).jpg",
		// Video part of a Pixel motion photo
		"PXL_0001.MP", "PXL_0001.MP.jpg", "PXL_0001.MP.jpg.supplemental-metadata.json",
	)

	tests := []struct {
		media   string
		sidecar string
	}{
		{"20160918_071839-edited(1).jpg", "20160918_071839.jpg.supplemental-metadata.json"},
		{"IMG_0248(1).MP4", "IMG_0248.HEIC.supplemental-metadata.json"},
		{"DSC_0001(1).jpg", ""},
		{"PXL_0001.MP", "PXL_0001.MP.jpg.supplemental-metadata.json"},
	}
	for _, test := range tests {
		got, err := FindSidecar(filepath.Join(dir, test.media))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != test.sidecar && !(got == "" && test.sidecar == "") {
			t.Errorf("FindSidecar(%q) = %q; want %q", test.media, filepath.Base(got), test.sidecar)
		}
	}
}

func TestFindSidecarInPrefersExactMatchElsewhere(t *testing.T) {
	// The folder of the photo only has the sidecar of the HEIC with the same name, the exact
	// sidecar of the JPEG is in the same folder of another takeout
	root := createTree(t,
		"Takeout 1/Google Photos/Photos from 2023/IMG_0248.jpeg",
		"Takeout 1/Google Photos/Photos from 2023/IMG_0248.HEIC.supplemental-metadata.json",
		"Takeout 2/Google Photos/Photos from 2023/IMG_0248.jpeg.supplemental-metadata.json",
	)
	dirs := []string{
		filepath.Join(root, "Takeout 1", "Google Photos", "Photos from 2023"),
		filepath.Join(root, "Takeout 2", "Google Photos", "Photos from 2023"),
	}
	got, err := FindSidecarIn(dirs, filepath.Join(dirs[0], "IMG_0248.jpeg"))
	if err != nil || filepath.Base(got) != "IMG_0248.jpeg.supplemental-metadata.json" {
		t.Errorf("FindSidecarIn = %q, %v; want the exact sidecar from the other takeout", got, err)
	}
}

func TestCorrectedFileName(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, header []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(header, make([]byte, 32)...), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	png := []byte("\x89PNG\r\n\x1a\n")
	heic := []byte{0, 0, 0, 0x28, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}
	webp := []byte("RIFF\x00\x00\x00\x00WEBP")
	mp4 := []byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}

	tests := []struct {
		name   string
		header []byte
		want   string
	}{
		{"photo.png", jpeg, "photo.jpg"},
		{"photo.HEIC", jpeg, "photo.jpg"},
		{"photo.webp", jpeg, "photo.jpg"},
		{"FullSizeRender.jpg", heic, "FullSizeRender.heic"},
		{"real.png", png, "real.png"},
		{"real.jpeg", jpeg, "real.jpeg"},
		{"real.webp", webp, "real.webp"},
		{"clip.MOV", mp4, "clip.MOV"}, // video containers are left alone
	}
	for _, test := range tests {
		if got := CorrectedFileName(write(test.name, test.header)); got != test.want {
			t.Errorf("CorrectedFileName(%q) = %q; want %q", test.name, got, test.want)
		}
	}
}

func TestEditedCopyWithCounterDoesNotTakeAnotherPhotosSidecar(t *testing.T) {
	// Six different film scans all called 000008.JPG, two edited copies of the first one
	dir := createFiles(t,
		"000008.JPG", "000008.JPG.supplemental-metadata.json",
		"000008(1).JPG", "000008.JPG.supplemental-metadata(1).json",
		"000008-edited.JPG", "000008-edited.JPG.supplemental-metadata.json",
		"000008-edited(1).JPG",
		"000008(1)-edited.JPG",
	)
	for _, test := range []struct{ media, sidecar string }{
		{"000008(1).JPG", "000008.JPG.supplemental-metadata(1).json"},
		{"000008-edited.JPG", "000008-edited.JPG.supplemental-metadata.json"},
		{"000008-edited(1).JPG", "000008.JPG.supplemental-metadata.json"}, // falls back to the original, not to 000008(1)
		{"000008(1)-edited.JPG", "000008.JPG.supplemental-metadata(1).json"},
	} {
		got, _ := FindSidecar(filepath.Join(dir, test.media))
		if filepath.Base(got) != test.sidecar {
			t.Errorf("FindSidecar(%q) = %q; want %q", test.media, filepath.Base(got), test.sidecar)
		}
	}
}

func TestZeroCounterIsPartOfTheName(t *testing.T) {
	dir := createFiles(t,
		"20170307_213818.jpg", "20170307_213818.jpg.supplemental-metadata.json",
		"20170307_213818(0).jpg", "20170307_213818(0).jpg.supplemental-metadata.json",
	)
	for _, test := range []struct{ media, sidecar string }{
		{"20170307_213818.jpg", "20170307_213818.jpg.supplemental-metadata.json"},
		{"20170307_213818(0).jpg", "20170307_213818(0).jpg.supplemental-metadata.json"},
	} {
		got, _ := FindSidecar(filepath.Join(dir, test.media))
		if filepath.Base(got) != test.sidecar {
			t.Errorf("FindSidecar(%q) = %q; want %q", test.media, filepath.Base(got), test.sidecar)
		}
	}
}

func TestNameWithCounter(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want string
	}{
		{"IMG_0001.jpg", 0, "IMG_0001.jpg"},
		{"IMG_0001.jpg", 1, "IMG_0001(1).jpg"},
		{"IMG_0001(1).jpg", 1, "IMG_0001(2).jpg"}, // continues instead of "(1)(1)"
		{"IMG_0001(3).HEIC", 2, "IMG_0001(5).HEIC"},
		{"FullSizeRender(1).heic", 1, "FullSizeRender(2).heic"},
	}
	for _, test := range tests {
		if got := NameWithCounter(test.name, test.n); got != test.want {
			t.Errorf("NameWithCounter(%q, %d) = %q; want %q", test.name, test.n, got, test.want)
		}
	}
}
