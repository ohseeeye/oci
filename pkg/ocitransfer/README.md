# ocitransfer

Copy images, indexes, artifacts, and tagged repositories between OCI backends.
Blob transfer helpers work through `oci.Reader` and `oci.Writer`: downloads
fetch ranges in parallel and expose an ordered stream; uploads write sequential
chunks and commit the content digest.

## Example

Upload a blob to an in-memory registry, then download it to standard output.
The same helpers work with `ociclient` and other registry implementations.

```go
package main

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocitransfer"
)

func main() {
	ctx := context.Background()
	registry := ocimem.New()

	// The caller owns source; UploadBlob never closes it.
	source := strings.NewReader("example blob\n")
	desc, err := ocitransfer.UploadBlob(ctx, registry, "example", source,
		&ocitransfer.UploadOptions{ChunkSize: 1 << 20})
	if err != nil {
		panic(err)
	}

	blob, err := ocitransfer.DownloadBlob(ctx, registry, "example", desc.Digest,
		&ocitransfer.DownloadOptions{Concurrency: 4})
	if err != nil {
		panic(err)
	}
	defer blob.Close()

	// Read to EOF to verify both the size and digest.
	if _, err := io.Copy(os.Stdout, blob); err != nil {
		panic(err)
	}
}
```

## Options and lifecycle

Pass `nil` options to use defaults. Zero-valued fields also select defaults.

- `DownloadOptions`: six concurrent ranges, adaptive chunk sizing (4–256 MiB),
  three attempts per range, and a 30-second timeout per attempt. Set `ChunkSize`
  for fixed-size ranges or `MaxAttempts: 1` to disable retries.
- `UploadOptions`: 100 MiB chunks and SHA-256 digests by default. Set `ChunkSize`
  to control buffering or `Algorithm` to select another supported digest.

Downloads require concurrent, context-aware range reads. Range errors and final
verification errors are returned while reading the stream. Always close the
download reader, including on error; closing it or canceling the context stops
prefetching. Stopping before EOF does not verify the complete blob.

Failed uploads are canceled. Writes are not replayed because the registry may
already have accepted some bytes. To resume an upload explicitly, use
`oci.Writer.PushBlobChunkedResume`. Cancellation is checked between source reads;
a blocking source must provide its own cancellation mechanism.

## Copy an image, index, or artifact

`Copy` transfers all content reachable from one tag or digest between any source
`oci.Reader` and destination `oci.ReadWriter`. It preserves raw manifest bytes,
digests, annotations, and all platforms. Children are copied before parents,
and destination tags are assigned after the content succeeds.

```go
// Preserve the source tag "release" at the destination.
desc, err := ocitransfer.Copy(ctx,
    source, "team/app", "release",
    destination, "mirror/app", nil)
if err != nil {
    return err
}

// Copy a pinned digest and explicitly assign a different destination tag.
_, err = ocitransfer.Copy(ctx,
    source, "team/app", desc.Digest.String(),
    destination, "mirror/app", &ocitransfer.CopyOptions{
        Tags: []string{"production"},
        Concurrency: 4,
        IncludeReferrers: true,
    })
```

Both endpoints can be HTTP clients,
in-memory registries, layouts, SQLite registries, or middleware wrappers. The
caller owns both endpoints.

`CopyOptions` controls:

- `Tags`: nil preserves a source tag; digest references remain untagged. An
  explicitly empty slice copies by digest only. Non-empty tags rename or add
  destination references. An empty source reference is always an error.
- `Concurrency`: maximum simultaneous blob transfers; zero defaults to four.
- `MountFrom`: optional repository **in the destination registry** from which to
  try blob mounts. Missing blobs or unsupported mounts fall back to streaming.
  Other mount errors fail the copy. Mounting is opt-in.
- `IncludeReferrers`: recursively copy artifacts referring to reachable manifests,
  such as signatures and SBOMs. Unsupported discovery is returned as an error.
  Subject references themselves are not dependencies and are not traversed.
- `MaxManifestSize`: per-manifest buffering limit; zero defaults to 16 MiB.

Blobs stream with size and digest verification. Existing destination blobs are
skipped. Existing destination manifests are still traversed so missing children
in a sparse destination are filled. Inline descriptor data is materialized;
blobs with URLs and no inline data remain external references, and their URLs
are never fetched. Unknown manifest media types return `oci.ErrUnsupported`
because their dependency structure is unknown.

Copies are additive and not atomic. Failure can leave completed content or some
assigned tags; rerunning skips existing content. The helper does not retry or
resume failed uploads automatically and does not perform platform filtering.

## Copy all tags in a repository

Use a separate helper to explicitly request repository scope:

```go
err := ocitransfer.CopyRepository(ctx,
    source, "team/app",
    destination, "mirror/app",
    &ocitransfer.CopyOptions{IncludeReferrers: true})
```

The source implements `oci.Registry` for tag enumeration. Every listed tag is
resolved once when processed, its content is copied, and its name is preserved.
Shared content is deduplicated across tags. `CopyOptions.Tags` must be nil.
Destination-only tags are retained, and the operation is not a transactional
snapshot of the source.

“All tags” does not include untagged roots: the registry interface has no way to
enumerate all manifests. Reachable children and optionally referrers are included.
