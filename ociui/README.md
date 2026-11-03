# Registry Explorer

This is dreg's local copy of the dagdotdev Explorer. Support packages are scoped
under `internal/explore/internal/`; no sibling checkout or local module replacement
is required. See [UPSTREAM.md](UPSTREAM.md) for provenance, local changes, and
license locations. The server imports `github.com/ohseeeye/dreg/internal/explore`.

## One required registry

The explorer consumes one `github.com/ohseeeye/oci` `oci.Registry`:

```go
handler := explore.New(registry)
```

`New` panics for a nil registry. The caller owns backend construction,
authentication, transport settings, middleware, and backend lifetime. There is no
default HTTP client, host resolver, or transport option inside the explorer.

All references are **repository-relative**:

```text
/                                           # catalog and browsing forms
/team/widget/tags/list                      # tags
/team/widget                                # tags for now (repository landing page)
/team/widget:v1                             # manifest by tag
/team/widget@sha256:...                      # manifest by digest
/team/widget/blobs/sha256:...                # structured/text/binary/image preview
/team/widget/blobs/sha256:.../etc/file       # file inside an auto-detected archive
/team/widget/referrers/sha256:...            # referrers
/team/widget/_oci/tag-history/v1             # optional tag history
/fs/team/widget@sha256:.../                  # layer filesystem (unchanged)
/layers/team/widget@sha256:.../              # merged filesystem (unchanged)
```

Nested repository names are supported. Registry/artifact pages use
`oci/pkg/mux`; explicit resources precede pull references and the bare repository
fallback. Navigation links use the explicit `/tags/list` resource so they do not
depend on the repository landing page's future behavior. Resource suffixes take
precedence over ambiguous repository names; append `/tags/list` to explicitly
list a repository whose name itself ends in `/tags/list`.
Explicit `/fs/`, merged `/layers/`, size, cache, and raw endpoints keep their paths
and handlers.
Their top-level namespaces and assets are reserved by the Explorer. In dreg,
the outer `ociserver` router owns `/v2/...`, including its 404/405 responses;
these requests never reach the Explorer fallback.

Catalog GET forms submit to `/_open`, which validates the entered image or
repository and redirects to its canonical path without requiring JavaScript.
View options remain query parameters: `render`, repeated `jq` stages, media-type
hints, pagination, and history filters. Root query selectors and the singular
`/blob/` preview endpoint are not supported; there is no URL compatibility layer.
A registry without catalog support can still be browsed using a known repository
or image reference.

Tag manifest pages remain at `/<repo>:<tag>`; fetching a tag never redirects the
browser to its digest. The header exposes an immutable digest link separately.
Config links and merged-filesystem links pin to the fetched manifest's digest,
while view/filter links retain the tag path. Only digest-addressed manifest
pages receive immutable cache headers.

Names are passed to the backend unchanged. `ubuntu` means the repository
`ubuntu` in the supplied registry, not `docker.io/library/ubuntu`. Bare image
references entered in the image form select the `latest` tag; a bare **path**
lists tags instead. Dotted namespace components are literal repository components:
`example.com/team/widget` does **not** switch registries.
Host:port references and HTTP URLs are not accepted as image references.

References use `ociref.ParseRelative` from `ohseeeye/oci` v0.1.2. It parses
registry-relative names with an empty `Host`, without Docker normalization or a
synthetic authority. The explorer supplies `latest` only when looking up an image
or tag history with no explicit tag or digest. External-registry PURL qualifiers
are displayed as data instead of linking them into the local registry namespace.

## Blob previews

`/<repo>/blobs/<digest>` is the common blob preview entry point, not a JSON-only
route. Structured JSON/config views retain `jq`, history, certificate, and binary
render options. Other blobs use the existing text/binary/image/archive previews;
without a useful media-type hint, a bounded prefix is sniffed and replayed through
the same reader. Archive descendants stay under `/<repo>/blobs/<digest>/<path>`.
Image bytes still use the sandboxed, image-only `/raw/` endpoint.

Blob repository captures cannot contain `:` or `@`, and the entire digest is
validated before serving content. The digest therefore fixes the object boundary:
`/namespace/fs/blobs/<digest>/fs/file` belongs to repository `namespace/fs`, and
an archive filename containing `blobs/<another-digest>` cannot change the object
being read. Blob descendant routes precede other UI resources. Filesystem adapters
also expose their validated archive root to the directory renderer rather than
having it guess that boundary from filenames. Dot-segment traversal is rejected.

## Backends and authentication

An in-memory registry needs no HTTP server:

```go
backend := ocimem.New() // Populate artifacts before browsing.
handler := explore.New(backend)
```

HTTP clients are constructed outside the explorer:

```go
config, err := ociauth.Load(nil) // Optional; nil Config uses anonymous auth.
if err != nil {
    return err
}
client, err := ociclient.New("ghcr.io", &ociclient.Options{
    UserAgent: "my-artifact-browser",
    Transport: ociauth.NewStdTransport(ociauth.StdTransportParams{Config: config}),
})
if err != nil {
    return err
}
handler := explore.New(client)
```

`ociclient`, `ociauth`, and `ocimem` are packages in `github.com/ohseeeye/oci`.
An OCI layout or middleware-composed registry can also be supplied directly.
The explorer does not require an HTTP-specific capability or a client wrapper.

dreg passes its configured SQLite backend directly to `explore.New`; the
Explorer browses the same repositories as `/v2/`. It does not accept separate
registry/authentication CLI flags. For other callers constructing an HTTP backend,
configure transport, credentials, and user agent on `ociclient` before supplying
it to the Explorer. The Explorer itself has no transport or user-agent options.

## Browsing and caching

Tag/catalog pagination uses `n` for page size and the last returned name as the
`next` cursor. Referrers are rendered as an OCI index assembled from the registry
iterator. Optional tag-history pages use `oci.TagHistory`, with `n`, `before`,
and `since` filters; there is no vendor-host special case.

Clicking a manifest's size opens a size-sorted filesystem listing at `/sizes/`.
For a multi-platform OCI index or Docker manifest list, this first shows a
platform directory; selecting a platform opens that child manifest's file
listing. Platform-directory byte counts are manifest-document sizes, not
unpacked filesystem totals. Opening the directory does not download every
platform's layers. APK package ownership is optional enrichment: missing package
databases do not prevent listings, and database readers are closed after use.

Registry images use a same-origin `/raw/` endpoint, so the browser needs neither
registry credentials nor signed URLs. Raw responses use private cache headers,
`nosniff`, and a sandbox CSP. All artifact reads go through the supplied
registry. Direct `/http/` and `/https/` archive-browsing routes are not supported;
external descriptor URLs are displayed without proxying downloads.

The explorer has no ping/token cache or global manifest cache. Apply registry
middleware for appropriately scoped content caching. Archive indexes are cached
by content digest on disk when `CACHE_DIR` is set; TOCs are cached in memory.
There is no GCS cache backend. SOCI checkpoints permit file access through lazy
`GetBlobRange` reads; cached directory listings fetch no blob bytes.

`ociclient` handles servers that ignore Range by skipping and limiting
full responses, so registry range reads need no application-side workaround.
Other supplied registries own their range correctness.

The explorer uses `ociref.Reference`, `oci.Descriptor`, `oci.Digest`, and
`oci.IndexOrManifest` directly, preserving original manifest bytes and backend
descriptors without model conversions. Media types are plain strings throughout
the explorer and SOCI interfaces, with common OCI/Docker constants supplied by
`oci` v0.1.2 rather than local copies. Legacy Docker schema-1 manifests use
`oci.Schema1Manifest` and the upstream media-type constants from v0.1.1.
Only the command fields inside the JSON-encoded `v1Compatibility` string and
modern image-config history are decoded with minimal local JSON shapes.
Debug logging uses structured `log/slog` calls; the copied ggcr logging shim has
been removed. HTTP compression uses the existing klauspost module with zstd
explicitly disabled to retain the previous gzip-only response policy.

Byte-size formatting belongs to the rendering layer: Explorer and the HTTP/archive
renderer share `internal/render.ByteSize`, using only the standard library. The
Explorer package tree has no SQLite or `go-humanize` dependency; dreg's database
backend is a caller-supplied implementation of `oci.Registry`, not a UI dependency.
Readers compose standard I/O interfaces with their original closers; decoder and
buffer owners retain explicit close/flush behavior rather than no-op closers.
Directory-cache range readers close their underlying files. Streaming HTML escaping
is also shared in `internal/render`, preserving flushing after every write. MIME
sniffing uses `net/http.DetectContentType`; unused native-file serving, Snappy
conversion, ZIP adapters, and flate encoder code have been removed from the forks.
The checkpoint-aware compression decoders used by SOCI remain local forks.

The `oci` dependency requires Go 1.25 or newer.
