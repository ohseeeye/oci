# Explorer source provenance

`internal/explore` is a temporary local copy of
[`github.com/jonjohnsonjr/dagdotdev/pkg/explore`](https://github.com/jonjohnsonjr/dagdotdev/tree/main/pkg/explore).
The Explorer files, embedded assets, lexer, tests, and golden fixtures were
already copied into dreg before its supporting packages were brought over.

The support code was copied on 2026-10-28 from the local dagdotdev working tree,
whose HEAD was `ce53794b563b13072ead1bbce3a3ca820a30f972`.
That checkout includes local changes; this is a working-tree snapshot, not a
claim that every file matches the committed upstream revision.

## Layout

- `internal/explore`: registry Explorer and its embedded assets/tests.
- `internal/explore/lexer`: query lexer used by the interpreter.
- `internal/explore/internal/gguf`: GGUF model inspection.
- `internal/explore/internal/gzip`, `zstd`: compressed-stream helpers.
- `internal/explore/internal/render`: shared, standard-library-only presentation helpers.
- `internal/explore/internal/xxd`: binary previews.
- `internal/explore/internal/soci`: seekable archive indexing and filesystem reads.
- `internal/explore/internal/forks`: the required HTTP, ELF, and gzip/flate/zstd
  forks, including their transitive compression packages.

Only the transitive package closure needed by Explorer and its tests was copied,
including platform-specific Go/assembly files and support-package tests. Other
commands and unrelated dagdotdev packages were not imported.

## Local changes

Imports previously rooted at `github.com/jonjohnsonjr/dagdotdev/pkg/explore`
now point to `github.com/ohseeeye/dreg/internal/explore`. Other dagdotdev `../pkg`
imports now point to `github.com/ohseeeye/dreg/internal/explore/internal/`.
Modified Go files carry a relocation notice and retain existing copyright
headers. Formatting was normalized with `gofmt`; no functionality was intentionally
changed during relocation. Test golden files and embedded assets were retained.

The remaining compression forks expose checkpoint/state functionality used by
SOCI; they are not interchangeable with the standard library or an unmodified
compression module. The registry backend uses the regular
`github.com/klauspost/compress` module for stored manifest compression.

### Logging and HTTP compression cleanup

The copied `ggcr/logs` shim was removed. Its debug calls now use structured
`log/slog` logging, disabled at the default INFO level and following the
application's configured default logger. No ggcr implementation remains.

HTTP response compression now uses `github.com/klauspost/compress/gzhttp` from the
existing versioned module. `compression.go` explicitly disables zstd, since the
upstream wrapper now enables it by default while the copied wrapper was gzip-only.
The copied `gzhttp` package, its writer packages, and its private gzip/flate
dependencies were removed. The SOCI compression forks were not changed.

Before deletion, old/new middleware were compared across 25 cases, checking
status, headers, decoded bodies, and flush behavior. All matched. The comparison
log is `reports/import-runs/explore-gzip-compatibility.log`; permanent policy,
streaming, response-controller, and debug-logging tests remain in this package.
Compressed byte layout is not an equivalence requirement.

There is no dagdotdev module requirement or filesystem `replace`. The server
imports the local Explorer directly. All remaining external dependencies are
ordinary versioned Go modules declared in the root `../go.mod`.

### I/O composition and rendering helpers

The copied `and` package was removed. Buffered and limited readers now compose
`io.Reader` with the original `io.Closer` directly. Private ownership wrappers in
the gzip/zstd helpers close the decoder before its source, retaining the original
source-error policy. The SOCI index writer closes gzip before flushing its buffer
and still joins both errors, attempting both cleanup steps. These replacements do
not substitute `io.NopCloser` for real resource cleanup. Compression convenience
functions and archive features were retained.

The byte formatter formerly in `humanize` now lives in `internal/render` as
`ByteSize`. Explorer and the HTTP/archive renderer both depend on this shared
presentation layer, avoiding an import cycle. The optional header-size helper
moved from `explore.go` to `render.go`; zero still means an absent header field,
whereas actual zero-byte counts still render as `0 B`.

The shared formatter depends only on `fmt` and `math`. No `go-humanize` dependency
was introduced: Explorer itself has no SQLite or `go-humanize` imports and can be
built without dreg's database backend. The root module's pre-existing indirect
`go-humanize` requirement remains solely for SQLite's dependency chain.

Before removal, the old and new formatters matched across 10,193 comparison
cases (powers of two and adjacent values, plus deterministic pseudo-random
values). Evidence is in `reports/import-runs/explore-byte-size-compatibility.log`.
Permanent tests cover IEC output, rounding, large counts, omitted header sizes,
source close/error forwarding, decoder/source close order, gzip finalization,
and both index-writer cleanup errors.

### Unused fork APIs and shared streaming rendering

The HTTP fork's unused native-filesystem `Dir` and `ServeFile` APIs were removed,
which also removed the copied `safefilepath` package. Archive filesystem adapters,
open-error mapping, and symlink path validation remain. Content sniffing now uses
`net/http.DetectContentType` with the same 512-byte sniff limit; the copied
`sniff.go` was removed. Before deletion, both sniffers matched across 11,026
signature, truncation, boundary, and deterministic random-input cases. Evidence
is in `reports/import-runs/explore-sniff-compatibility.log`.

The forked zstd's unused Snappy converter and ZIP adapters were removed, along
with the converter's private `snapref` dependency. Unused flate encoder code and
token tables were removed as well. The checkpoint-aware decoders, platform and
assembly implementations, and compression convenience helpers remain intact.
SOCI tests now round-trip tar, gzip, and zstd archives through stored indexes,
including extraction using restored mid-stream deflate checkpoints. These larger
fixtures exposed a pre-existing zstd sniffing race: an asynchronous sniff decoder
could still be reading from the buffered prefix while the indexer reused its
storage. Zstd sniffing now uses a synchronous decoder to finish before that reuse.

The three identical streaming HTML-escaping writers now share
`internal/render.NewEscapeWriter`. It preserves per-write flushing, consumed-byte
counts, and destination-error propagation. Duplicate, unreachable archive-sniffing
helpers in `blobs.go` and unused local rendering helpers were deleted. The cache's
custom multi-error implementation was replaced with `errors.Join`, retaining
ordered messages and all constituent errors. Directory-cache range readers now
close their underlying files instead of using a no-op closer. Permanent tests
cover streaming escape output and failures, MIME sniffing and preview rendering,
archive symlink path validation, range-reader ownership, and joined cache errors.
The broader cache abstractions have not been redesigned.

### Registry UI paths

Registry/artifact browsing now uses the existing `oci/pkg/mux` router and
repository-relative pull-reference paths instead of root query dispatch. Tags,
blobs, referrers, and optional tag history have explicit resource paths. Catalog
forms submit to a dedicated `/_open` navigation handler. No legacy query dispatch
or URL compatibility redirects are retained.
Shared URL builders update descriptor, annotation, PURL, header, and pagination
links consistently. Tag pages retain their visible identity while immutable
config/filesystem associations use the resolved digest. Filesystem/size endpoint
paths and checkpoint decoding are unchanged. Golden fixtures were regenerated
for these link changes. Routing tests cover nested names, validation, precedence,
reserved namespaces, form navigation, and the real UI/API router composition.
Filesystem namespaces mount directly on the same router without a secondary
ServeMux for old slash/path-cleaning behavior. Tests request canonical URLs
directly, without authority rewriting or automatic redirect following.

### Size listings and optional package metadata

Manifest size links distinguish OCI indexes/Docker manifest lists from leaf
images. Index size pages list platforms with links to each child's size-sorted
filesystem view instead of treating the index as an image with no layers.
Archive filesystems now synthesize only real implicit directories, returning
`fs.ErrNotExist` for missing files. The optional APK ownership lookup verifies
that the database is a regular file and closes its reader; it no longer tries
to scan a synthetic directory for a missing database. Empty size listings show
an explicit empty state. Regression tests follow the actual manifest-size and
platform links and cover optional APK metadata and implicit directories.

### Unified blob previews

`/<repo>/blobs/<digest>/...` is the sole blob preview entry point; the singular
`/blob/` handler and its URL parser support were removed. The blob handler supports
both structured JSON views and generic text/binary/image/archive previews.
Unknown types are sniffed through a bounded buffer without a second blob fetch,
retaining the original descriptor and closer. Existing explicit
layer, merged-filesystem, size, and raw endpoint paths are unchanged.

Constrained repository and digest captures prevent archive descendants from being
reinterpreted as another repository or UI resource. Directory adapters expose an
optional archive-root boundary so flat listings do not infer it from '@' characters
in file paths. Regression tests cover nested names including `namespace/fs`,
resource-like archive paths, SHA-256/SHA-512 boundaries, tar/gzip/zstd archives,
preview selection, reader reuse, and source closing. Golden fixtures use the
canonical blob links and the catalog forms' dedicated submission endpoint.

## Licenses

The source project's Apache-2.0 license is retained as `LICENSE`. Existing
per-file attribution notices are preserved. Additional fork licenses are retained:

- Go-derived forks: `internal/forks/LICENSE-Go` (BSD-style).
- Klaus Post compression forks:
  `internal/forks/github.com/klauspost/compress/LICENSE`.
- Upstream HTTP compression: the regular module's `gzhttp/LICENSE` (Apache-2.0),
  bundled in Docker as `/usr/share/licenses/dreg/gzhttp.LICENSE`.
- xxhash:
  `internal/forks/github.com/klauspost/compress/zstd/internal/xxhash/LICENSE.txt` (MIT).

The Docker runtime includes these notices under `/usr/share/licenses/dreg/`.

## Validation

```sh
go test -race ./...
go vet ./...
go build -o bin/dreg .
docker build --target runtime -t dreg:runtime .
```

The runtime target verifies a standalone container build without embedding the
large snapshot collection. Omit `--target runtime` to build the normal image
with `snapshots/repositories/` included. Neither build needs a sibling checkout.
