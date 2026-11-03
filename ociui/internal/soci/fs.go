// Modified for dreg: local imports; see internal/explore/UPSTREAM.md.

package soci

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	httpserve "github.com/ohseeeye/dreg/internal/explore/internal/forks/http"
)

// More than enough for FileServer to Peek at file contents.
const bufferLen = 2 << 16

var up *sociDirEntry = &sociDirEntry{nil, "..", nil, "", "", 0}

type RenderDir func(w http.ResponseWriter, r *http.Request, fname string, prefix string, mediaType string, size int64, ref string, f httpserve.File, ctype string) error

type MultiFS struct {
	fss    []*SociFS
	prefix string

	lastFs   *SociFS
	lastFile string

	ref string

	render RenderDir
	mt     string
	size   int64
}

func NewMultiFS(fss []*SociFS, prefix string, ref string, size int64, mt string, render RenderDir) *MultiFS {
	filtered := []*SociFS{}
	for _, fs := range fss {
		if fs != nil {
			filtered = append(filtered, fs)
		}
	}
	return &MultiFS{
		fss:    filtered,
		prefix: prefix,
		ref:    ref,
		render: render,
		size:   size,
		mt:     mt,
	}
}

func (s *MultiFS) ArchiveRoot() string { return "/" + strings.Trim(s.prefix, "/") }

func (s *MultiFS) RenderHeader(w http.ResponseWriter, r *http.Request, fname string, f httpserve.File, ctype string) error {
	slog.Debug("render merged filesystem header", "last_file", s.lastFile, "last_fs_nil", s.lastFs == nil, "name", fname)
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.IsDir() {
		return s.render(w, r, fname, s.prefix, s.mt, s.size, s.ref, f, ctype)
	}

	if s.lastFs == nil {
		return fmt.Errorf("something went wrong")
	}
	return s.lastFs.RenderHeader(w, r, fname, f, ctype)

}

func (s *MultiFS) err(name string) fs.File {
	return &multiFile{
		fs:   s,
		name: name,
	}
}

func (s *MultiFS) chase(original string, gen int) (*TOCFile, *SociFS, error) {
	slog.Debug("resolve merged filesystem path", "name", original, "generation", gen)
	if original == "" {
		return nil, nil, fmt.Errorf("empty string")
	}
	if gen > 64 {
		log.Printf("chase(%q) aborting at gen=%d", original, gen)
		return nil, nil, fmt.Errorf("too many symlinks")
	}
	for _, sfs := range s.fss {
		chased, next, err := sfs.chase(original, gen)
		if err == fs.ErrNotExist {
			if next != original && next != "" {
				return s.chase(next, gen+1)
			}
			continue
		}
		return chased, sfs, err
	}

	return nil, nil, fs.ErrNotExist
}

func (s *MultiFS) find(name string) (*TOCFile, *SociFS, error) {
	slog.Debug("find merged filesystem entry", "name", name)
	needle := path.Clean("/" + name)
	for _, sfs := range s.fss {
		for _, fm := range sfs.files {
			if path.Clean("/"+fm.Name) == needle {
				slog.Debug("merged filesystem entry found", "name", fm.Name, "size", fm.Size)
				return &fm, sfs, nil
			}
		}
	}

	return nil, nil, fs.ErrNotExist
}

func (s *MultiFS) Everything() ([]fs.DirEntry, error) {
	sum := 0
	for _, sfs := range s.fss {
		sum += len(sfs.files)
	}
	have := map[string]string{}
	whiteouts := map[string]struct{}{}
	des := make([]fs.DirEntry, 0, sum)
	for i, sfs := range s.fss {
		layerWhiteouts := map[string]struct{}{}
		for _, fm := range sfs.files {
			fm := fm
			sde := sfs.dirEntry("", &fm)
			name := path.Base(fm.Name)
			dir := path.Dir(fm.Name)
			fullname := path.Join(dir, name)

			sde.layerIndex = i
			wn := path.Join(dir, ".wh..wh..opq")
			if _, sawOpaque := whiteouts[wn]; sawOpaque {
				slog.Debug("merged filesystem opaque whiteout", "name", wn)
				sde.whiteout = wn
			} else {
				wn := path.Join(dir, ".wh."+name)
				if _, ok := whiteouts[wn]; ok {
					slog.Debug("skip whiteout entry", "name", name, "whiteout", wn)
					sde.whiteout = wn
				} else if source, ok := have[fullname]; ok {
					if sde.IsDir() {
						continue
					}
					slog.Debug("entry overwritten by another layer", "name", name, "source", source)
					sde.overwritten = source
				} else if strings.HasPrefix(name, ".wh.") {
					layerWhiteouts[fullname] = struct{}{}
				}
			}

			if sde.IsDir() {
				continue
			}
			// Whiteout markers (.wh.X, .wh..wh..opq) drive the layer-merge
			// logic above but shouldn't surface as visible entries.
			if strings.HasPrefix(name, ".wh.") {
				continue
			}
			have[fullname] = sfs.ref
			des = append(des, sde)
		}
		for k := range layerWhiteouts {
			whiteouts[k] = struct{}{}
		}
	}
	return des, nil
}

func (s *MultiFS) Open(original string) (fs.File, error) {
	s.lastFile = original
	slog.Debug("open merged filesystem entry", "name", original)
	name := strings.TrimPrefix(original, s.prefix)

	chunks := strings.Split(name, " -> ")
	name = chunks[len(chunks)-1]
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "."
	}
	slog.Debug("open resolved merged filesystem entry", "name", name)

	fm, sfs, err := s.find(name)
	if err != nil {
		slog.Debug("open merged filesystem entry failed", "name", name, "error", err)

		base := path.Base(name)
		if base == "index.html" || base == "favicon.ico" {
			return nil, fs.ErrNotExist
		}

		fm, sfs, err = s.chase(name, 0)
		if err != nil {
			if sfs == nil {
				if err == fs.ErrNotExist {
					for _, layer := range s.fss {
						if layer.hasDirectory(name) {
							return s.err(name), nil
						}
					}
					// An empty merged filesystem still has a root directory.
					if path.Clean("/"+name) == "/" {
						return s.err(name), nil
					}
				}
				return nil, &fs.PathError{Op: "open", Path: original, Err: err}
			}
			s.lastFs = sfs
			return sfs.err(name), nil
		}

		if fm.Typeflag == tar.TypeDir {
			slog.Debug("resolved merged filesystem directory", "name", name, "dir", fm.Name)
			return sfs.dir(fm), nil
		}

		name = path.Clean("/" + fm.Name)
	}
	s.lastFs = sfs

	if fm.Typeflag == tar.TypeDir {
		// Return a multifs dir file so we search everything
		slog.Debug("merged filesystem directory", "name", name, "dir", fm.Name)
		return s.dir(fm), nil
	}

	return &sociFile{fs: sfs, name: name, fm: fm}, nil
}

func (s *MultiFS) dir(fm *TOCFile) fs.File {
	return &multiFile{
		fs:   s,
		name: fm.Name,
		fm:   fm,
	}
}

type multiFile struct {
	fs   *MultiFS
	name string
	fm   *TOCFile
}

func (s *multiFile) Stat() (fs.FileInfo, error) {
	slog.Debug("stat merged filesystem entry", "name", s.name)
	if s.fm != nil {
		s.fm.Name = strings.TrimPrefix(s.fm.Name, "./")
		s.fm.Name = strings.TrimPrefix(s.fm.Name, "/")
		slog.Debug("merged filesystem metadata", "name", s.fm.Name)
		return TarHeader(s.fm).FileInfo(), nil
	}

	// We don't have an entry, so we need to synthesize one.
	return &dirInfo{s.name}, nil
}

func (s *multiFile) Read(p []byte) (int, error) {
	slog.Debug("read merged filesystem entry", "name", s.name)
	return 0, &fs.PathError{Op: "read", Path: s.name, Err: fs.ErrInvalid}
}

func (s *multiFile) ReadDir(n int) ([]fs.DirEntry, error) {
	slog.Debug("read merged filesystem directory", "name", s.name)
	have := map[string]string{}
	realDirs := map[string]struct{}{}
	implicitDirs := map[string]*SociFS{}
	whiteouts := map[string]string{}
	de := []fs.DirEntry{}
	if s.name == "." || s.name == "/" || s.name == "" || s.name == "./" {
		slog.Debug("read merged filesystem root")
	} else {
		de = append(de, up)
	}
	subdir := strings.TrimSuffix(strings.TrimPrefix(s.name, "./"), "/")
	for i, sfs := range s.fs.fss {
		dc := sfs.readDir(subdir)
		for d := range dc.realDirs {
			realDirs[d] = struct{}{}
		}
		for d := range dc.implicitDirs {
			implicitDirs[d] = sfs
		}
		for _, got := range dc.entries {
			name := got.Name()
			if strings.HasPrefix(name, ".wh.") {
				slog.Debug("hide whiteout entry", "name", name)
				continue
			}

			sde, ok := got.(*sociDirEntry)
			if !ok {
				return nil, fmt.Errorf("this shouldn't happen: %q", name)
			}
			sde.layerIndex = i
			opq, sawOpaque := whiteouts[".wh..wh..opq"]
			if sawOpaque {
				slog.Debug("merged directory opaque whiteout", "name", opq)
				sde.whiteout = opq
			} else if wh, ok := whiteouts[".wh."+name]; ok {
				slog.Debug("skip whiteout directory entry", "name", name, "whiteout", wh)
				sde.whiteout = wh
			} else if source, ok := have[name]; ok {
				if sde.IsDir() {
					continue
				}
				slog.Debug("directory entry overwritten by another layer", "name", name, "source", source)
				sde.overwritten = source
				have[name] = sfs.ref
			} else {
				have[name] = sfs.ref
			}

			de = append(de, sde)
		}
		// Add whiteouts at the end because they don't apply to the current layer.
		for k, v := range dc.whiteouts {
			whiteouts[k] = v
		}
	}

	for dir, sfs := range implicitDirs {
		if _, ok := realDirs[dir]; !ok {
			slog.Debug("add implicit merged directory", "dir", dir)
			de = append(de, sfs.dirEntry(dir, nil))
		}
	}

	slog.Debug("merged directory entries", "name", s.name, "count", len(de))
	return de, nil
}

func (s *multiFile) Close() error {
	return nil
}

type BlobSeeker interface {
	Reader(ctx context.Context, off int64, end int64) (io.ReadCloser, error)
}

func FS(index Index, bs BlobSeeker, prefix string, ref string, maxSize int64, mt string, render RenderFunc) *SociFS {
	slog.Debug("new archive filesystem", "prefix", prefix, "ref", ref)
	fs := &SociFS{
		index:   index,
		bs:      bs,
		maxSize: maxSize,
		prefix:  prefix,
		ref:     ref,
		mt:      mt,
		render:  render,
	}
	if index != nil {
		if toc := index.TOC(); toc != nil {
			fs.files = toc.Files
		}
	}
	return fs
}

type RenderFunc func(w http.ResponseWriter, r *http.Request, fname string, prefix string, ref string, kind string, mediaType string, size int64, f httpserve.File, ctype string) error

type SociFS struct {
	files []TOCFile

	bs BlobSeeker

	index Index

	prefix  string
	ref     string
	maxSize int64

	mt string

	render RenderFunc
}

func (s *SociFS) ArchiveRoot() string { return "/" + strings.Trim(s.prefix, "/") }

func (s *SociFS) RenderHeader(w http.ResponseWriter, r *http.Request, fname string, f httpserve.File, ctype string) error {
	if s.render != nil {
		kind := "tar+gzip"
		if toc := s.index.TOC(); toc != nil && toc.Type != "" {
			kind = toc.Type
			if s.mt == "" {
				s.mt = toc.MediaType
			}
		}
		return s.render(w, r, fname, s.prefix, s.ref, kind, s.mt, s.index.TOC().Csize, f, ctype)
	}
	return nil
}

func (s *SociFS) extractFile(tf *TOCFile) (io.ReadCloser, error) {
	return ExtractFile(context.Background(), s.index, s.bs, tf)
}

// Tar archives need not contain explicit directory headers. Synthesize a
// directory only for the root or a path with actual descendants, never for an
// arbitrary missing file (e.g. an optional package database).
func (s *SociFS) hasDirectory(name string) bool {
	dir := path.Clean("/" + name)
	if dir == "/" {
		return true
	}
	for _, file := range s.files {
		if strings.HasPrefix(path.Clean("/"+file.Name), dir+"/") {
			return true
		}
	}
	return false
}

func (s *SociFS) err(name string) fs.File {
	return &sociFile{
		fs:   s,
		name: name,
	}
}

func (s *SociFS) dir(fm *TOCFile) fs.File {
	return &sociFile{
		fs:   s,
		name: fm.Name,
		fm:   fm,
	}
}

func (s *SociFS) Open(original string) (fs.File, error) {
	slog.Debug("open archive entry", "name", original)
	name := strings.TrimPrefix(original, s.prefix)

	chunks := strings.Split(name, " -> ")
	name = chunks[len(chunks)-1]
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "."
	}
	slog.Debug("open resolved archive entry", "name", name)

	fm, err := s.find(name)
	if err != nil {
		slog.Debug("open archive entry failed", "name", name, "error", err)

		base := path.Base(name)
		if base == "index.html" || base == "favicon.ico" {
			return nil, fs.ErrNotExist
		}

		chased, _, err := s.chase(name, 0)
		if err != nil {
			if err == fs.ErrNotExist && s.hasDirectory(name) {
				return s.err(name), nil
			}
			return nil, &fs.PathError{Op: "open", Path: original, Err: err}
		}

		if chased.Typeflag == tar.TypeDir {
			return s.dir(chased), nil
		}

		name = path.Clean("/" + chased.Name)
		fm = chased
	}

	return &sociFile{fs: s, name: name, fm: fm}, nil
}

func (s *SociFS) ReadDir(original string) ([]fs.DirEntry, error) {
	slog.Debug("read archive directory", "name", original)
	dir := strings.TrimPrefix(original, s.prefix)
	if dir != original {
		slog.Debug("read resolved archive directory", "name", dir)
	}

	dc := s.readDir(original)
	if dir == "." || dir == "/" || dir == "" || dir == "./" {
		slog.Debug("read archive root")
	} else {
		dc.entries = append(dc.entries, s.dirEntry("..", nil))
	}

	for dir := range dc.implicitDirs {
		if _, ok := dc.realDirs[dir]; !ok {
			slog.Debug("add implicit archive directory", "dir", dir)
			dc.entries = append(dc.entries, s.dirEntry(dir, nil))
		}
	}

	slog.Debug("archive directory entries", "name", dir, "count", len(dc.entries))
	return dc.entries, nil
}

func (s *SociFS) Everything() ([]fs.DirEntry, error) {
	des := make([]fs.DirEntry, 0, len(s.files))
	for _, fm := range s.files {
		fm := fm
		des = append(des, s.dirEntry("", &fm))
	}
	return des, nil
}

type dirContent struct {
	entries      []fs.DirEntry
	realDirs     map[string]struct{}
	implicitDirs map[string]struct{}
	whiteouts    map[string]string
}

func (s *SociFS) readDir(original string) *dirContent {
	slog.Debug("build archive directory listing", "name", original)
	dir := strings.TrimPrefix(original, s.prefix)
	if dir != original {
		slog.Debug("resolved archive listing path", "name", dir)
	}

	dc := &dirContent{
		entries:      []fs.DirEntry{},
		implicitDirs: map[string]struct{}{},
		realDirs:     map[string]struct{}{},
		whiteouts:    map[string]string{},
	}

	prefix := path.Clean("/" + dir)

	for _, fm := range s.files {
		fm := fm
		name := path.Clean("/" + fm.Name)

		base := path.Base(name)
		if base == ".wh..wh..opq" {
			if strings.HasPrefix(prefix, path.Dir(name)) {
				slog.Debug("add opaque whiteout", "base", base, "name", name)
				dc.whiteouts[base] = name
			}
		} else if strings.HasPrefix(base, ".wh.") {
			if prefix == path.Dir(name) {
				slog.Debug("add whiteout", "base", base, "name", name)
				dc.whiteouts[base] = name
			}
		}

		if prefix != "/" && name != prefix && !strings.HasPrefix(name, prefix+"/") {
			continue
		}

		fdir := path.Dir(strings.TrimPrefix(name, prefix))
		if !(fdir == "/" || (fdir == "." && prefix == "/")) {
			if fdir != "" && fdir != "." {
				if fdir[0] == '/' {
					fdir = fdir[1:]
				}
				implicit := strings.Split(fdir, "/")[0]
				if implicit != "" {
					dc.implicitDirs[implicit] = struct{}{}
				}
			}
			continue
		}

		if fm.Typeflag == tar.TypeDir {
			dirname := s.dirEntry(dir, &fm).Name()
			if dirname[0] == '/' {
				dirname = dirname[1:]
			}
			dc.realDirs[dirname] = struct{}{}
		}
		dc.entries = append(dc.entries, s.dirEntry(dir, &fm))
	}

	return dc
}

func (s *SociFS) find(name string) (*TOCFile, error) {
	slog.Debug("find archive entry", "name", name)
	needle := path.Clean("/" + name)
	for _, fm := range s.files {
		if path.Clean("/"+fm.Name) == needle {
			slog.Debug("archive entry found", "name", fm.Name, "size", fm.Size)
			return &fm, nil
		}
	}

	return nil, fs.ErrNotExist
}

// todo: cache symlinks to require fewer iterations?
// todo: or maybe symlinks as separate list?
func (s *SociFS) chase(original string, gen int) (*TOCFile, string, error) {
	if original == "" {
		return nil, "", fmt.Errorf("empty string")
	}
	if gen > 64 {
		log.Printf("chase(%q) aborting at gen=%d", original, gen)
		return nil, "", fmt.Errorf("too many symlinks")
	}

	name := path.Clean("/" + original)
	dir := path.Dir(name)
	dirs := []string{dir}
	if dir != "" && dir != "." {
		prev := dir
		// Walk up to the first directory.
		for next := prev; next != "." && filepath.ToSlash(next) != "/"; prev, next = next, filepath.Dir(next) {
			dirs = append(dirs, strings.TrimPrefix(next, "/"))
		}
	}

	for _, fm := range s.files {
		fm := fm
		if fm.Name == original || fm.Name == name {
			if fm.Typeflag == tar.TypeSymlink {
				return s.chase(fm.Linkname, gen+1)
			}
			return &fm, "", nil
		}
		if fm.Typeflag == tar.TypeSymlink {
			for _, dir := range dirs {
				if fm.Name == dir {
					// todo: re-fetch header.Linkname/<rest>

					// lib/libgo.so.23.0.0
					// usr/lib64/libgo.so.23.0.0
					// usr/lib/libgo.so.23.0.0

					if strings.HasPrefix(fm.Linkname, "/") {
						prefix := path.Clean("/" + fm.Name)
						next := path.Join(fm.Linkname, strings.TrimPrefix(name, prefix))
						log.Printf("chase(%q): refetch(%q); fm.Name=%q, fm.Linkname=%q, name=%q", original, next, fm.Name, fm.Linkname, name)
						return s.chase(next, gen+1)
					} else {
						prefix := path.Clean("/" + fm.Name)
						linkdir := path.Dir(fm.Name)
						targetdir := path.Join(linkdir, fm.Linkname)
						next := path.Join(targetdir, strings.TrimPrefix(name, prefix))
						log.Printf("chase(%q): refetch(%q); fm.Name=%q, fm.Linkname=%q, name=%q, linkdir=%q, targetdir=%q", original, next, fm.Name, fm.Linkname, name, linkdir, targetdir)
						return s.chase(next, gen+1)
					}
				}
			}
		}
	}

	return nil, original, fs.ErrNotExist
}

type sociFile struct {
	fs     *SociFS
	name   string
	fm     *TOCFile
	buf    *bufio.Reader
	closer func() error
}

func (s *sociFile) Stat() (fs.FileInfo, error) {
	if s.fm == nil {
		// We don't have an entry, so we need to synthesize one.
		log.Printf("Stat(%q) has no fm", s.name)
		return &dirInfo{s.name}, nil
	}

	if s.fm.Typeflag == tar.TypeSymlink {
		hdr := TarHeader(s.fm)
		hdr.Typeflag = tar.TypeDir
		return hdr.FileInfo(), nil
	}

	return TarHeader(s.fm).FileInfo(), nil
}

func (s *sociFile) Read(p []byte) (int, error) {
	if s.fm == nil || s.fm.Size == 0 {
		return 0, io.EOF
	}
	if s.buf == nil {
		rc, err := s.fs.extractFile(s.fm)
		if err != nil {
			slog.Debug("extract archive file failed", "error", err, "file", s.fm)
			return 0, fmt.Errorf("extractFile: %w", err)
		}
		s.closer = rc.Close

		if len(p) <= bufferLen {
			s.buf = bufio.NewReaderSize(rc, bufferLen)
		} else {
			s.buf = bufio.NewReaderSize(rc, len(p))
		}
	}
	return s.buf.Read(p)
}

func (s *sociFile) Seek(offset int64, whence int) (int64, error) {
	return 0, fmt.Errorf("not implemented")
}

func (s *sociFile) ReadDir(n int) ([]fs.DirEntry, error) {
	slog.Debug("read archive file directory")
	if s.fm != nil && s.fm.Typeflag == tar.TypeSymlink {
		fm := *s.fm
		fm.Name = "."

		return []fs.DirEntry{
			s.fs.dirEntry("..", nil),
			s.fs.dirEntry("", &fm),
		}, nil
	}
	return s.fs.ReadDir(s.name)
}

func (s *sociFile) Size() int64 {
	if s.fm == nil {
		return 0
	}
	return s.fm.Size
}

func (s *sociFile) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer()
}

func (s *SociFS) dirEntry(dir string, fm *TOCFile) *sociDirEntry {
	return &sociDirEntry{
		fs:  s,
		dir: dir,
		fm:  fm,
	}
}

type sociDirEntry struct {
	fs  *SociFS
	dir string
	fm  *TOCFile

	// If set, the whiteout file that deleted this.
	whiteout string
	// If set, the file that overwrote this file.
	overwritten string

	// Set by multifs for sorting overwritten files
	layerIndex int
}

func (s *sociDirEntry) Name() string {
	if s.fm == nil {
		return s.dir
	}
	trimmed := strings.TrimPrefix(s.fm.Name, "./")
	if s.dir != "" && !strings.HasPrefix(s.dir, "/") && strings.HasPrefix(trimmed, "/") {
		trimmed = strings.TrimPrefix(trimmed, "/"+s.dir+"/")
	} else {
		trimmed = strings.TrimPrefix(trimmed, s.dir+"/")
	}
	return path.Clean(trimmed)
}

func (s *sociDirEntry) IsDir() bool {
	if s.fm == nil {
		return true
	}
	return s.fm.Typeflag == tar.TypeDir
}

func (s *sociDirEntry) Type() fs.FileMode {
	if s.fm == nil {
		return (&dirInfo{s.dir}).Mode()
	}
	return TarHeader(s.fm).FileInfo().Mode()
}

func (s *sociDirEntry) Info() (fs.FileInfo, error) {
	if s.fm == nil {
		return &dirInfo{s.dir}, nil
	}
	return TarHeader(s.fm).FileInfo(), nil
}

func (s *sociDirEntry) Layer() string {
	if s.fs == nil {
		return ""
	}
	return s.fs.ref
}

func (s *sociDirEntry) Whiteout() string {
	return s.whiteout
}

func (s *sociDirEntry) Overwritten() string {
	return s.overwritten
}

func (s *sociDirEntry) Index() int {
	return s.layerIndex
}

// If we don't have a file, make up a dir.
type dirInfo struct {
	name string
}

func (f dirInfo) Name() string       { return f.name }
func (f dirInfo) Size() int64        { return 0 }
func (f dirInfo) Mode() os.FileMode  { return os.ModeDir }
func (f dirInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (f dirInfo) IsDir() bool        { return true }
func (f dirInfo) Sys() interface{}   { return nil }
