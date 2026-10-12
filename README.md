# ohseeeye/oci

Go packages for building, serving, and composing OCI (Open Container Initiative) registries.

The top-level package (`oci`) defines the [Registry interface](./interface.go) for reading blobs and manifests, pushing content, listing tags, and other registry operations.

Full reference documentation can be found at [pkg.go.dev/github.com/ohseeeye/oci](https://pkg.go.dev/github.com/ohseeeye/oci).

The aim is to provide an ergonomic interface for defining and layering OCI registry implementations.

This project is in v0; breaking API and import-path changes are expected.

This project is an independent hard fork of [docker/oci](https://github.com/docker/oci).
It is not intended as a drop-in replacement. Its history also includes
[cue-labs/oci](https://github.com/cue-labs/oci) and
[go-containerregistry](https://pkg.go.dev/github.com/google/go-containerregistry/pkg/registry).

## Purpose

The goal is to make registry implementations easy to build and compose through
a shared interface. Backends can store content in memory or on disk, middleware
can restrict or route operations, and HTTP adapters connect registries to the
OCI distribution protocol.

The core module's library packages use only the Go standard library at runtime.
`ocisqlite` is a separate Go module with a SQLite dependency; `cmd/ocisrv` also
has its own module. External dependencies in the core module support tests.

## Packages

| Package | Description |
|---------|-------------|
| `oci` | Core interface (`oci.Registry`) and types shared across all packages. |
| `ociclient` | HTTP client that implements `oci.Registry` against a remote OCI registry. |
| `ocilayout` | Filesystem-backed `oci.Registry` implementation for OCI Image Layout directories, including shared and per-repository layouts. |
| [`ocisqlite`](./ocisqlite/README.md) | Persistent registry with SQLite metadata and shared blob files; a separate Go module. |
| `ocimem` | Lightweight in-memory `oci.Registry` implementation, useful for testing and caching. |
| [`ocimiddleware`](./ocimiddleware/README.md) | Registry wrappers for read-only and immutable views, namespace prefixes, access control, repository routing, read-through caching, and operation logging. |
| `ociserver` | HTTP server that serves the OCI distribution protocol on top of any `oci.Registry`. |
| `ociunify` | Combines two registries into a single unified `oci.Registry`, with configurable read policy. |
| `pkg/dockerhub` | Docker Hub hostnames for reference normalization, registry connections, and credential lookup. |
| `pkg/mux` | General-purpose HTTP routing with path templates and middleware. |
| `pkg/ociauth` | Authentication transport implementing the Docker/OCI token flow, plus helpers for loading credentials from Docker config files. |
| `pkg/ocidigest` | OCI-compatible content digest calculation, validation, and streaming verification. |
| `pkg/ociref` | OCI reference parsing, validation, and normalization. |
| `pkg/ocitransfer` | Streaming blob transfers through `oci.Reader` and `oci.Writer`, with adaptive parallel downloads and sequential chunked uploads. |

`oci.TagHistory` is a separate, experimental capability that callers can check
with a type assertion on an `oci.Registry`. `ocimem`, `ocilayout`, and `ocisqlite` implement it,
and `ociserver` serves the proposed tag-history endpoint when its backend does.
`ociclient` implements the same capability against remote registries; an
upstream 404 reports `oci.ErrUnsupported`.

The server currently passes the [OCI distribution conformance tests](https://pkg.go.dev/github.com/opencontainers/distribution-spec/conformance).

Experimental conditional manifest tag updates
use `If-Match` and `oci.PushManifestParameters.IfMatch` to protect updates against
concurrent writers. Memory and layout backends support the condition; HTTP
clients forward it after discovering a digest ETag on a tag GET or HEAD.
Middleware forwards it to storage. Other implementations may ignore it.

## Sparse manifests

Memory, layout (including per-repository layouts), and SQLite backends expose
`AllowSparseManifests` in their constructor options. It defaults to false. Enable
it to accept manifests before their child content arrives and to evict referenced
children independently. Manifest structure, descriptors, and digests are still
validated; missing children return not found until uploaded.

## Pull-through caching

Wrap an upstream registry with `ocimiddleware.Cache(upstream, cache, options)`. Enable
`AllowSparseManifests` on the cache backend so manifests can arrive before their
children. Memory, layout (including per-repository layouts), and SQLite backends
support this option; it defaults to false and retains manifest validation.

Digest content is cached automatically. Tags always resolve upstream unless
`ocimiddleware.CacheOptions.CacheTags` is enabled; tag expiration belongs to the backend.
Listings and referrers always go upstream. See [cache middleware](./ocimiddleware/README.md#cache)
for lifecycle, invalidation, and configuration details.

## Usage

### List tags on Docker Hub

```go
package main

import (
	"context"
	"fmt"

	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/ohseeeye/oci/pkg/dockerhub"
)

func main() {
	cf, err := ociauth.Load(nil)
	if err != nil {
		panic(err)
	}

	ocl, err := ociclient.New(dockerhub.ReferenceHost, &ociclient.Options{
		Transport: ociauth.NewStdTransport(ociauth.StdTransportParams{
			Config: cf,
		}),
	})
	if err != nil {
		panic(err)
	}

	tags := ocl.Tags(context.Background(), "library/alpine", nil)
	for tag, err := range tags {
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s\n", tag)
	}
}
```

### Fetch a manifest by tag

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/ohseeeye/oci/pkg/dockerhub"
)

func main() {
	cf, err := ociauth.Load(nil)
	if err != nil {
		panic(err)
	}

	ocl, err := ociclient.New(dockerhub.ReferenceHost, &ociclient.Options{
		Transport: ociauth.NewStdTransport(ociauth.StdTransportParams{
			Config: cf,
		}),
	})
	if err != nil {
		panic(err)
	}

	// Fetch the image index or manifest for alpine:latest.
	r, err := ocl.GetTag(context.Background(), "library/alpine", "latest")
	if err != nil {
		panic(err)
	}
	defer r.Close()

	fmt.Printf("media type: %s\n", r.Descriptor().MediaType)
	fmt.Printf("digest:     %s\n", r.Descriptor().Digest)

	var manifest oci.IndexOrManifest
	if err := json.NewDecoder(r).Decode(&manifest); err != nil {
		panic(err)
	}
	if manifest.Config != nil {
		fmt.Printf("config digest: %s\n", manifest.Config.Digest)
	}
	for i, child := range manifest.Manifests {
		fmt.Printf("manifest[%d]: %s (%d bytes)\n", i, child.Digest, child.Size)
	}
	for i, layer := range manifest.Layers {
		fmt.Printf("layer[%d]: %s (%d bytes)\n", i, layer.Digest, layer.Size)
	}
}
```

### Pull a blob (image layer)

```go
package main

import (
	"context"
	"io"
	"os"

	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/ohseeeye/oci/pkg/dockerhub"
)

func main() {
	cf, err := ociauth.Load(nil)
	if err != nil {
		panic(err)
	}

	ocl, err := ociclient.New(dockerhub.ReferenceHost, &ociclient.Options{
		Transport: ociauth.NewStdTransport(ociauth.StdTransportParams{
			Config: cf,
		}),
	})
	if err != nil {
		panic(err)
	}

	// Pull a specific blob by digest.
	const repo = "library/alpine"
	const layerDigest = "sha256:bca4290a96390d7a6fc6f2f9929370d06f8dfcacba591c76e3d5c5044e7f420c"

	blob, err := ocl.GetBlob(context.Background(), repo, layerDigest)
	if err != nil {
		panic(err)
	}
	defer blob.Close()

	f, err := os.Create("layer.tar.gz")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	if _, err := io.Copy(f, blob); err != nil {
		panic(err)
	}
}
```

### Transfer blobs

See the [ocitransfer README](./pkg/ocitransfer/README.md) for streaming upload and
parallel download examples using any `oci.Reader` or `oci.Writer`.

### Serve a local in-memory registry over HTTP

The `ocisrv` command starts the same kind of in-memory registry for quick
local testing. Content is held in memory and is lost when the process exits.
It listens on `localhost:5000` by default; use `-listen` to choose another address.
Run it from its module directory:

```sh
cd cmd/ocisrv
go run . -listen localhost:5000
```

```go
package main

import (
	"net/http"

	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociserver"
)

func main() {
	backend := ocimem.New()
	handler, err := ociserver.New(backend, nil)
	if err != nil {
		panic(err)
	}
	if err := http.ListenAndServe("localhost:5000", handler); err != nil {
		panic(err)
	}
}
```

## Testing

Run the root module tests from the repository root:

```sh
go test ./...
```

The SQLite backend and server command have their own `go.mod`, so test them separately:

```sh
(cd ocisqlite && go test ./...)
(cd cmd/ocisrv && go test ./...)
```

`task test` and `task lint` include both the core and SQLite modules.

Run the OCI distribution conformance tests with Docker installed and running:

```sh
task conformance
```

See [internal/conformance/README.md](./internal/conformance/README.md) for backend selection, direct Go commands, and shared reports.
