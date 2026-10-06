# ocisqlite

`ocisqlite` implements `oci.Registry` and the experimental `oci.TagHistory`
capability using SQLite for metadata and files for blob and manifest content.
It uses [zombiezen/go-sqlite](https://github.com/zombiezen/go-sqlite), imported as
`zombiezen.com/go/sqlite`, without CGo or `database/sql`.

This directory is a separate Go module so the core OCI module does not acquire
a SQLite runtime dependency. Its local `replace` directive uses the core module
in the parent directory during development.

The release workflow tags this module with `ocisqlite/vX.Y.Z`, separately from
the core module's `vX.Y.Z` tags. SQLite is tagged on pushes to `main` only when
the push changes files under `ocisqlite/`. It uses the same patch default and
`(MINOR)` / `(MAJOR)` commit-message tokens as the core module. The first
SQLite release defaults to `ocisqlite/v0.0.1`. Consumers request its version
without the directory prefix:

```sh
go get github.com/ohseeeye/oci/ocisqlite@v0.0.1
```

```go
backend, err := ocisqlite.New("./registry", nil)
if err != nil {
    return err
}
defer backend.Close()

handler, err := ociserver.New(backend, nil)
if err != nil {
    return err
}
// Serve handler with your HTTP server.
_ = handler
```

Import `github.com/ohseeeye/oci/ocisqlite` and
`github.com/ohseeeye/oci/ociserver` for this example. `Options.PoolSize` controls
the SQLite connection pool; zero selects four connections. Close registry
instances when finished, after their operations and upload writers have stopped.

## Storage

```text
registry/
  metadata.db
  metadata.db-wal       # managed by SQLite while open
  metadata.db-shm       # managed by SQLite while open
  blobs/
    sha256/<encoded digest>
    sha512/<encoded digest>
  uploads/<session>
```

All repositories share content by digest. SQLite records repository membership,
manifests, blob and index references, annotations, tags, history, and upload
sessions. Mounting a blob adds membership without copying content. An upload is
scoped to its repository and can resume across registry restarts. Its ID is
opaque; an offset of `-1` resumes at the committed offset.

The schema is in [schema.sql](./schema.sql), versioned through SQLite's
`user_version`. Foreign keys are enabled on every connection. Tags and history
are updated in the same transaction; history keeps descriptor snapshots after
tag and manifest deletion. Event timestamps are UTC with nanosecond precision
and increase strictly for each repository/tag pair.

Known OCI and Docker manifests and indexes are validated. Their children must
already exist in the same repository; subjects may be missing, and externally
hosted layers need not be stored locally. Unknown manifest media types are
stored as opaque content, matching the other backends. Referrers include the
manifest's annotations and artifact type, falling back to the config media type.

## Transactions and lifecycle

Conditional manifest pushes accept `PushManifestParameters.IfMatch` for exactly
one tag, using the quoted digest returned in its ETag. SQLite compares the
current digest and updates the tag and history in the same immediate write
transaction, including across registry instances sharing the database. A
mismatch returns `oci.ErrManifestInvalid` (`400` / `MANIFEST_INVALID` over HTTP),
without publishing content or changing metadata. Lists, weak ETags, and `*` are
not supported; an empty condition leaves ordinary pushes unconditional.

Blob uploads stream to a temporary file, verify size and digest, sync their
content, and publish through an atomic hard link before metadata is committed.
Conditional manifest content is published after its condition has passed,
while holding the write transaction.
Both directories must be on the same local filesystem. Duplicate uploads are
still verified. Failed metadata commits may leave an unreferenced content file,
but do not make unpublished content visible through the registry.

SQLite uses WAL mode with full synchronization. Multiple registry instances can
share a directory. Metadata writes and each upload file modification are
serialized by SQLite transactions. Concurrent handles for the same upload must
have the current offset; a stale handle fails rather than overwriting content.
Each successful `Write` syncs the file before committing its new offset. Resume
discards bytes beyond that offset left by an interrupted write. Commit verifies
the completed upload while holding the write transaction, so a large upload's
final verification temporarily delays other writers.

Deleting a blob removes only that repository's membership. Blobs referenced by
manifests, and manifests referenced by indexes, cannot be deleted until their
parents are removed. Deleting a manifest removes its tags and records deletion
events, while retaining its blob membership. Deleting a tag preserves the
manifest. Repositories remain present after their contents are deleted.

Content files and global blob metadata are retained after membership deletion.
Automatic garbage collection and cleanup of abandoned uploads or orphan files
are not implemented. Backups must capture SQLite and the content files together;
stop writes and close the registry before copying its directory. An open WAL
database cannot be backed up safely by copying only `metadata.db`.

## Testing

From this directory:

```sh
go test -race ./...
go test -tags=integration ./... -run TestOCIConformance -count=1 -v
```

Conformance tests use the repository's shared Docker harness. From the repository
root, `task conformance:ocisqlite` runs just this backend; `task conformance`
includes it alongside `ocimem` and `ocilayout`.
