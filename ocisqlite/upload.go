package ocisqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ohseeeye/oci"
	"zombiezen.com/go/sqlite"
)

// PushBlobChunked starts a persistent upload in repo.
func (r *Registry) PushBlobChunked(ctx context.Context, repo string, chunkSize int) (oci.BlobWriter, error) {
	return r.PushBlobChunkedResume(ctx, repo, "", 0, chunkSize)
}

// PushBlobChunkedResume resumes a repository-scoped upload. An offset of -1
// selects the last committed offset. Unknown sessions are never created here.
func (r *Registry) PushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (oci.BlobWriter, error) {
	if offset < -1 {
		return nil, oci.ErrRangeInvalid
	}
	if chunkSize <= 0 {
		chunkSize = 8 * 1024
	}
	create := id == ""
	if create {
		id = newUploadID()
	} else if !validUploadID(id) {
		return nil, oci.ErrBlobUploadUnknown
	}
	path := filepath.Join(r.dir, "uploads", id)
	var size int64
	createdFile := false
	err := r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		repoID, err := repository(conn, repo, create)
		if err != nil {
			return err
		}
		if create {
			if offset > 0 {
				return oci.ErrRangeInvalid
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				return err
			}
			createdFile = true
			if err := f.Close(); err != nil {
				return err
			}
			if err := syncDir(filepath.Dir(path)); err != nil {
				return err
			}
			now := time.Now().UnixNano()
			return execute(conn, `INSERT INTO upload(session,repository_id,created_at,last_upload_at) VALUES (?,?,?,?)`, id, repoID, now, now)
		}
		size, err = uploadSize(conn, repoID, id)
		if err != nil {
			return err
		}
		if offset >= 0 && offset != size {
			return oci.ErrRangeInvalid
		}
		f, err := openUpload(path, size)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil {
		if createdFile {
			_ = os.Remove(path)
		}
		return nil, err
	}
	return &blobWriter{registry: r, ctx: ctx, repo: repo, id: id, path: path, size: size, chunkSize: chunkSize}, nil
}

func uploadSize(conn *sqlite.Conn, repo int64, id string) (int64, error) {
	size := int64(-1)
	err := rows(conn, "SELECT size FROM upload WHERE repository_id=? AND session=?", func(s *sqlite.Stmt) error { size = s.ColumnInt64(0); return nil }, repo, id)
	if err != nil {
		return 0, err
	}
	if size < 0 {
		return 0, oci.ErrBlobUploadUnknown
	}
	return size, nil
}

// The caller holds an IMMEDIATE transaction, serializing upload file changes
// across registry instances and processes. Extra bytes are an interrupted write;
// only bytes whose offset committed in SQLite belong to the upload.
func openUpload(path string, size int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600) // #nosec G703 -- validated upload ID
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && info.Size() < size {
		err = fmt.Errorf("upload file shorter than committed offset")
	}
	if err == nil && info.Size() > size {
		err = f.Truncate(size)
		if err == nil {
			err = f.Sync()
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

type blobWriter struct {
	mu               sync.Mutex
	registry         *Registry
	ctx              context.Context
	repo, id, path   string
	size             int64
	chunkSize        int
	closed, finished bool
}

func (w *blobWriter) Size() int64    { w.mu.Lock(); defer w.mu.Unlock(); return w.size }
func (w *blobWriter) ChunkSize() int { return w.chunkSize }
func (w *blobWriter) ID() string     { return w.id }

func (w *blobWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.finished {
		return 0, fmt.Errorf("upload closed")
	}
	var n int
	err := w.registry.withConn(w.ctx, true, func(conn *sqlite.Conn) error {
		repo, err := repository(conn, w.repo, false)
		if err != nil {
			return err
		}
		size, err := uploadSize(conn, repo, w.id)
		if err != nil {
			return err
		}
		if size != w.size {
			return oci.ErrRangeInvalid
		}
		f, err := openUpload(w.path, size)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err = f.WriteAt(p, size)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return execute(conn, "UPDATE upload SET size=?,last_upload_at=? WHERE session=?", size+int64(n), time.Now().UnixNano(), w.id)
	})
	if err != nil {
		return 0, err
	}
	w.size += int64(n)
	return n, nil
}

func (w *blobWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *blobWriter) Commit(digest oci.Digest) (oci.Descriptor, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return oci.Descriptor{}, oci.ErrBlobUploadUnknown
	}
	w.closed = true
	var desc oci.Descriptor
	err := w.registry.withConn(w.ctx, true, func(conn *sqlite.Conn) error {
		repo, err := repository(conn, w.repo, false)
		if err != nil {
			return err
		}
		size, err := uploadSize(conn, repo, w.id)
		if err != nil {
			return err
		}
		if size != w.size {
			return oci.ErrRangeInvalid
		}
		f, err := openUpload(w.path, size)
		if err != nil {
			return err
		}
		defer f.Close()
		desc, err = writeBlob(w.ctx, w.registry.dir, oci.Descriptor{Digest: digest, Size: size}, f)
		if err != nil {
			return err
		}
		if err := addBlob(conn, repo, desc); err != nil {
			return err
		}
		return execute(conn, "DELETE FROM upload WHERE session=?", w.id)
	})
	if err != nil {
		return oci.Descriptor{}, err
	}
	w.finished = true
	// Failure to remove a now-unreferenced staging file does not undo a commit.
	_ = os.Remove(w.path)
	return desc, nil
}

func (w *blobWriter) Cancel() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil
	}
	w.closed = true
	// Cleanup still works after the upload's request context has been canceled.
	err := w.registry.withConn(context.WithoutCancel(w.ctx), true, func(conn *sqlite.Conn) error {
		repo, err := repository(conn, w.repo, false)
		if err != nil {
			return err
		}
		_, err = uploadSize(conn, repo, w.id)
		if errors.Is(err, oci.ErrBlobUploadUnknown) {
			return nil
		}
		if err != nil {
			return err
		}
		return execute(conn, "DELETE FROM upload WHERE repository_id=? AND session=?", repo, w.id)
	})
	if err != nil {
		return err
	}
	w.finished = true
	if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
