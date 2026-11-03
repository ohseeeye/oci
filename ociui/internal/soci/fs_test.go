package soci

import (
	"archive/tar"
	"errors"
	"io/fs"
	"testing"
)

func TestOpenMissingFilesAndImplicitDirectories(t *testing.T) {
	layer := &SociFS{files: []TOCFile{
		{Name: "etc/hello.txt", Typeflag: tar.TypeReg, Size: 5},
		{Name: "empty/", Typeflag: tar.TypeDir},
	}}
	for _, tc := range []struct {
		name string
		fsys fs.FS
	}{
		{"layer", layer},
		{"merged", NewMultiFS([]*SociFS{layer}, "", "", 0, "", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"lib/apk/db/installed", "etc/missing", "missing/tree", "et"} {
				file, err := tc.fsys.Open(name)
				if file != nil {
					file.Close()
				}
				if !errors.Is(err, fs.ErrNotExist) || file != nil {
					t.Errorf("Open(%q) = (%v, %v), want fs.ErrNotExist", name, file, err)
				}
			}
			for _, name := range []string{".", "/", "etc", "empty"} {
				file, err := tc.fsys.Open(name)
				if err != nil {
					t.Fatalf("Open directory %q: %v", name, err)
				}
				info, err := file.Stat()
				file.Close()
				if err != nil || !info.IsDir() {
					t.Errorf("Stat directory %q = (%v, %v)", name, info, err)
				}
			}
			file, err := tc.fsys.Open("etc/hello.txt")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if info, err := file.Stat(); err != nil || info.IsDir() || info.Size() != 5 {
				t.Fatalf("regular file metadata = (%v, %v)", info, err)
			}
		})
	}
}

func TestEmptyMergedFilesystemRoot(t *testing.T) {
	fsys := NewMultiFS(nil, "", "", 0, "", nil)
	root, err := fsys.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if entries, err := root.(fs.ReadDirFile).ReadDir(-1); err != nil || len(entries) != 0 {
		t.Fatalf("empty root listing = (%v, %v)", entries, err)
	}
	if file, err := fsys.Open("lib/apk/db/installed"); file != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file in empty filesystem = (%v, %v)", file, err)
	}
}
