# ocitransfer

Streaming blob transfers through `oci.Reader` and `oci.Writer`. Downloads fetch
ranges in parallel and expose an ordered stream; uploads write sequential chunks
and commit the content digest. Neither helper requires a full `oci.Registry`.

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
