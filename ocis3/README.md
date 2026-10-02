# ocis3

An experimental `oci.Registry` backed entirely by S3 objects, including
`oci.TagHistory`. No metadata database, local staging files, or aggregate index
file is required. This is a separate Go module using the AWS SDK for Go v2.

## Usage

Create the bucket first. Supply an S3 client configured with your credentials,
region, and any custom endpoint or path-style addressing requirements:

```go
cfg, err := config.LoadDefaultConfig(ctx)
if err != nil {
    return err
}
backend, err := ocis3.New(s3.NewFromConfig(cfg), "registry", &ocis3.Options{
    Prefix: "oci",
})
if err != nil {
    return err
}
handler, err := ociserver.New(backend, nil)
```

The imports in this example are `github.com/aws/aws-sdk-go-v2/config`,
`github.com/aws/aws-sdk-go-v2/service/s3`, `github.com/ohseeeye/oci/ocis3`, and
`github.com/ohseeeye/oci/ociserver`. Add the SDK config module to your application
if using its default credential and region discovery.

The client and bucket belong to the caller. The backend needs GET, HEAD, PUT,
DELETE, LIST, multipart creation/upload/completion/abort, and permission to use
conditional writes. The object store must support `If-Match` and `If-None-Match`
on PUT, `If-Match` on GET, `If-None-Match` on multipart completion, range reads, and strongly
consistent reads/listing. Integration tests use MinIO; AWS S3 has not been tested
by this prototype.

## Object layout

Keys below the configured prefix are versioned with `v1/`. Repository names can
contain slashes; reserved components begin with `_`, which is not a valid start
of a repository name component.

```text
v1/catalog/<repo>                                      repository marker
v1/blobs/<algorithm>/<digest>                          shared content
v1/repositories/<repo>/_blobs/<algorithm>/<digest>      membership marker
v1/repositories/<repo>/_manifests/<algorithm>/<digest>  manifest bytes
v1/repositories/<repo>/_tags/<tag>                      current event pointer
v1/repositories/<repo>/_tag_history/<tag>/<event-id>     immutable history event
v1/repositories/<repo>/_referrers/<subject-algorithm>/<subject-digest>/<algorithm>/<digest>
v1/repositories/<repo>/_uploads/<session>/session       committed offset/chunks
v1/repositories/<repo>/_uploads/<session>/chunks/<id>   immutable upload chunk
```

Manifest media types are stored as object `Content-Type`. Referrer objects hold
individual descriptors with artifact types and annotations. Listings use S3
prefixes and continuation tokens. Mounts add a membership marker without
copying content.

## Tag history and concurrent writers

Each history event contains its descriptor, event type, timestamp, and previous
event ID. A writer first creates the immutable event, then conditionally updates
the tag pointer using its previous ETag (or creates it with `If-None-Match: *`).
That pointer change commits both the assignment and history event. Conflicting
writers retry from the new pointer. History follows only reachable events, so
an unsuccessful writer's orphan event never appears in results.

Tag deletion publishes a tombstone and retains history. Manifest deletion also
records deletion events for its current tags. Timestamps strictly increase along
each chain, and history supports exclusive before/since bounds, digest filters,
and limits. An iterator takes a snapshot of the head pointer when iteration
starts, so later writes do not change its chain.

## Uploads

Direct pushes stream into a multipart upload; only verified size and digest
allow completion and repository membership publication. Transfer memory is
bounded by `Options.PartSize` (default 8 MiB, configurable from 5 to 64 MiB).
The maximum direct blob size is 10,000 configured parts.

Resumable writes publish immutable chunk objects and hash their bytes with
`ocidigest` for both SHA-256 and SHA-512. Each conditional session update commits
the chunk list, byte offset, and serialized digest state together. Failed writes
discard their advanced local hash state. A stale writer cannot overwrite another
writer's offset or digest state. Closing leaves the session resumable, including
across registry instances. Resume restores the persisted hash state and checks
that its offsets match the session and chunk records.

Commit verifies the requested SHA-256 or SHA-512 digest from that state before
freezing the session. A mismatch leaves an active session writable. Assembly
streams the recorded chunks into the final multipart blob without hashing them
again; conditional GETs check the ETag saved for each chunk, and byte counts are
still verified. Assembly still downloads and re-uploads the content. Commit then
marks the session complete and attempts chunk cleanup. It can be retried with
the same digest after a storage failure. A committing session cannot be canceled
because content publication may already be underway. Completed/canceled session
tombstones remain to reject stale writers. Sessions permit up to 10,000 chunks.

Digest state is mandatory for these new sessions; missing, inconsistent, or
unsupported state is rejected rather than reconstructed from chunks. Serialized
state is intended for the same library version and algorithm implementation, as
documented by `ocidigest`.

## Prototype limits

- S3 has no transaction across keys. Manifest bytes, referrer entries, and each
  tag commit separately. A failed push can leave a visible untagged manifest or
  a partial set of tag assignments; retrying the operation completes the work.
- Deletion protects referenced content by scanning repository manifests. This
  can be expensive and is not atomic with concurrent pushes. Serialize
  destructive maintenance with writers when reference safety is required.
- Concurrent updates to one tag are safe. Multi-tag updates, concurrent deletion
  and repush of the same manifest, and multiple simultaneous upload commits do
  not provide transactional guarantees.
- Blob deletion removes repository membership, retaining shared content. There
  is no garbage collector yet for unreferenced blobs, orphan history events,
  failed-write chunks, abandoned uploads, or session tombstones. Lifecycle rules
  must not expire reachable history or live upload chunks independently.
- A transport failure with an inconclusive readback may still have committed.
  Resume uploads to discover the durable offset; inspect tag state/history
  before retrying a tag write when avoiding duplicate assignment events matters.
- Repository markers persist after the last object is deleted. History traversal
  uses one GET per older event; very long histories will incur object requests.

## Testing

```sh
go test -race ./...
go test -tags=integration -count=1 -v ./...
```

Integration tests start isolated containers using `chainguard/minio` pinned to
its image digest and create a temporary bucket. Docker must be running. The
conformance harness uses the same pinned upstream runner as the other backends.
Run `task conformance:ocis3` from the repository root to retain reports under
`results/ocis3/`.

Release tags use `ocis3/vX.Y.Z` and are created only when a push to `main`
changes this directory. The local `replace github.com/ohseeeye/oci => ..`
directive keeps development and tests aligned with the root checkout.
