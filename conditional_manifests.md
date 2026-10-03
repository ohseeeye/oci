# Conditional manifest tag updates

This experimental extension lets clients update a mutable tag only if its
current manifest still matches a previously observed value. Ordinary pushes
without a condition keep their existing behavior.

## HTTP

Manifest GET and HEAD responses, addressed by either tag or digest, include a
strong `ETag` containing the quoted manifest digest, alongside
`Docker-Content-Digest`:

```http
HEAD /v2/example/app/manifests/latest

ETag: "sha256:<current-digest>"
Docker-Content-Digest: sha256:<current-digest>
```

Send that ETag when pushing a replacement:

```http
PUT /v2/example/app/manifests/latest
Content-Type: application/vnd.oci.image.index.v1+json
If-Match: "sha256:<current-digest>"
```

A supporting backend checks the condition as part of the tag update. Success
returns the ordinary `201 Created`, with the new digest and ETag. A stale value
or missing tag returns `412 Precondition Failed` with the experimental
`PRECONDITION_FAILED` error code. The tag and its history remain unchanged.

`If-Match` accepts a quoted ETag, a comma-separated list (any strong match
satisfies it), or `*` to require an existing tag. Weak validators such as
`W/"sha256:..."` never satisfy the condition. Malformed conditions return `400`.
An empty HTTP header is malformed, whereas an absent header is unconditional.

For this first version, conditional pushes require one tag in the request URL.
Digest-addressed PUTs and additional `?tag=` assignments with `If-Match` return
`400`; the condition cannot ambiguously apply to several tags. This extension
does not add `If-None-Match`, date-based conditions, or conditional manifest GETs.

The comparison tracks the current digest. If a tag changes from A to B and back
to A, a condition for A succeeds; it does not compare tag-history revisions.

## Go

`oci.PushManifestParameters.IfMatch` carries the HTTP field value, including its
quotes. A Go client can construct the value from a previously resolved digest:

```go
previous, err := registry.ResolveTag(ctx, "example/app", "latest")
if err != nil {
    return err
}
_, err = registry.PushManifest(ctx, "example/app", manifest, mediaType,
    &oci.PushManifestParameters{
        Tags:    []string{"latest"},
        IfMatch: strconv.Quote(previous.Digest.String()),
    })
if errors.Is(err, oci.ErrPreconditionFailed) {
    // Read the current tag and reconcile the conflicting update.
}
```

Direct conditional calls also require exactly one entry in `Tags`. An empty
`IfMatch` is unconditional. `ociclient` starts with conditional writes disabled.
A successful `GetTag` or `ResolveTag` response containing a strong ETag equal
to the quoted manifest digest enables them for that client instance and all its
repositories. `client.SupportsIfMatch()` reports this observation. Weak ETags,
unrelated validators, digest or blob reads, and PUT responses do not enable it.
The observation is safe to share between concurrent calls and lasts for the
lifetime of the client; later responses without ETags do not reset it.

Before discovery, `ociclient` omits a supplied `IfMatch` and pushes
unconditionally. After discovery, it forwards explicitly supplied conditions
in a single PUT to the tag URL; it does not attempt a bulk push or retry
unconditionally after a failure. It does not remember tag values or add
conditions to ordinary pushes. Callers must still supply the previous value
for the particular tag they want to update. A `412` HTTP response is recognizable with
`errors.Is(err, oci.ErrPreconditionFailed)`, including responses from other
servers that do not return our JSON error code.

## Backend support and concurrency

- `ocimem` compares and changes the tag under its registry mutex.
- Both `ocilayout` layouts compare under the existing instance mutex, before
  writing content, tag references, or history. The layout index and history
  are then saved together. Separate instances or processes sharing a layout
  directory do not share that mutex; concurrent external writers are not
  coordinated by this prototype.
- Routing and access middleware forward the parameters to their backend.
  `ociunify` forwards them to each registry independently; its existing writes
  are not a transaction across registries, so one side can succeed while the
  other fails.
- Third-party registry implementations can ignore the new optional field.
  `ociserver` passes the condition through without requiring a capability
  interface. A successful response therefore guarantees enforcement only when
  the caller knows the backend supports conditional writes. Digest ETags are
  our client's discovery convention, not proof that an arbitrary registry or
  backend enforces `If-Match` on PUT.

`oci.CheckManifestIfMatch` provides the shared comparison/parser for backend
implementations. Call it inside the lock or transaction that protects the tag
update, using an empty digest for an absent tag. A server-side resolve followed
by a separate push is insufficient to protect against concurrent writers.
