// Package ocisqlite implements an OCI registry with SQLite metadata and shared,
// digest-addressed files. It is a separate Go module from the core OCI library.
package ocisqlite

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed schema.sql
var schema string

// Options configures a registry's SQLite connection pool.
type Options struct {
	// PoolSize defaults to four when zero. Negative values are invalid.
	PoolSize int
}

// Registry implements [oci.Registry] and the experimental [oci.TagHistory].
// Close releases its database connections. Blob readers own their file handles.
type Registry struct {
	*oci.Funcs
	dir  string
	pool *sqlitex.Pool
}

var _ oci.Registry = (*Registry)(nil)
var _ oci.TagHistory = (*Registry)(nil)

// New opens or creates a registry rooted at dir. The directory contains
// metadata.db, blobs/<algorithm>/<digest>, and uploads/<session>.
// Multiple Registry instances may share a directory on a local filesystem.
func New(dir string, opts *Options) (*Registry, error) {
	if dir == "" {
		return nil, fmt.Errorf("directory must not be empty")
	}
	size := 4
	if opts != nil {
		if opts.PoolSize < 0 {
			return nil, fmt.Errorf("pool size must not be negative")
		}
		if opts.PoolSize > 0 {
			size = opts.PoolSize
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"blobs", "uploads"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o700); err != nil {
			return nil, err
		}
	}
	// Create with private permissions before SQLite opens the file.
	path := filepath.Join(abs, "metadata.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{
		Flags:    sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenWAL,
		PoolSize: size,
		PrepareConn: func(conn *sqlite.Conn) error {
			if err := execute(conn, "PRAGMA foreign_keys=ON"); err != nil {
				return err
			}
			return execute(conn, "PRAGMA synchronous=FULL")
		},
	})
	if err != nil {
		return nil, err
	}
	r := &Registry{dir: abs, pool: pool}
	err = r.withConn(context.Background(), true, func(conn *sqlite.Conn) error {
		version, err := integer(conn, "PRAGMA user_version")
		if err != nil {
			return err
		}
		switch version {
		case 0:
			return sqlitex.ExecuteScript(conn, schema, nil)
		case 1:
			return nil
		default:
			return fmt.Errorf("unsupported metadata schema version %d", version)
		}
	})
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	return r, nil
}

// Close releases the registry's SQLite connections. It must not run while
// registry operations are in progress; close upload writers first.
func (r *Registry) Close() error { return r.pool.Close() }

func (r *Registry) withConn(ctx context.Context, write bool, fn func(*sqlite.Conn) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := r.pool.Take(ctx)
	if err != nil {
		return err
	}
	defer r.pool.Put(conn)
	if write {
		end, beginErr := sqlitex.ImmediateTransaction(conn)
		if beginErr != nil {
			return beginErr
		}
		defer end(&err)
	} else {
		defer sqlitex.Transaction(conn)(&err)
	}
	err = fn(conn)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func execute(conn *sqlite.Conn, query string, args ...any) error {
	return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args})
}

func rows(conn *sqlite.Conn, query string, fn func(*sqlite.Stmt) error, args ...any) error {
	return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: fn})
}

func integer(conn *sqlite.Conn, query string, args ...any) (int64, error) {
	var n int64
	err := rows(conn, query, func(s *sqlite.Stmt) error { n = s.ColumnInt64(0); return nil }, args...)
	return n, err
}

func repository(conn *sqlite.Conn, name string, create bool) (int64, error) {
	if !ociref.IsValidRepository(name) || len(name) > 255 {
		return 0, oci.ErrNameInvalid
	}
	if create {
		if err := execute(conn, "INSERT INTO repository(name,created_at) VALUES (?,?) ON CONFLICT(name) DO NOTHING", name, time.Now().UnixNano()); err != nil {
			return 0, err
		}
	}
	id, err := integer(conn, "SELECT id FROM repository WHERE name=?", name)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, oci.ErrNameUnknown
	}
	return id, nil
}

func addBlob(conn *sqlite.Conn, repo int64, desc oci.Descriptor) error {
	now := time.Now().UnixNano()
	if err := execute(conn, "INSERT INTO blobs(digest,size,created_at) VALUES (?,?,?) ON CONFLICT(digest) DO NOTHING", desc.Digest.String(), desc.Size, now); err != nil {
		return err
	}
	size, err := integer(conn, "SELECT size FROM blobs WHERE digest=?", desc.Digest.String())
	if err != nil {
		return err
	}
	if size != desc.Size {
		return oci.ErrSizeInvalid
	}
	return execute(conn, "INSERT INTO repository_blob(repository_id,digest,created_at) VALUES (?,?,?) ON CONFLICT DO NOTHING", repo, desc.Digest.String(), now)
}
