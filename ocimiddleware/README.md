# ocimiddleware

Registry wrappers for access control, restricted views, repository routing,
read-through caching, and operation logging. Wrappers compose through `oci.Registry`.

## Cache

`ocimiddleware.Cache` wraps an upstream `oci.Registry` with another registry
as a read-through cache. It is part of the core module and adds no runtime dependencies.

```go
cache := ocimem.NewWithConfig(&ocimem.Config{AllowSparseManifests: true})
registry, err := ocimiddleware.Cache(upstream, cache, &ocimiddleware.CacheOptions{
    CacheTags: false, // default: resolve every tag upstream
})
if err != nil {
    return err
}
handler, err := ociserver.New(registry, nil)
```

Import `github.com/ohseeeye/oci/ocimiddleware`, `ocimem`, and `ociserver` for this
example. `upstream` can be any registry, including an `ociclient` connected to
a remote server. The caller owns and closes both backends.

### Reads

- Manifests and blobs are cached by repository and digest. The cache must accept
  parents before their children; enable `AllowSparseManifests` on `ocimem`,
  `ocilayout`, or `ocisqlite`.
- Tags are resolved upstream by default, then their content is read by digest.
  `CacheOptions.CacheTags` enables cached tag lookups. **Expiration belongs to the
  cache backend**: enabling this option with a backend without expiration keeps
  tags until they are changed or invalidated through this wrapper, or evicted
  directly from the cache. There is no built-in TTL.
- A cached tag lookup miss fetches the manifest to assign the tag because
  `oci.Registry` has no tag-only write. Digest-only HEAD misses forward upstream
  without fetching bodies.
- Tag and repository listings, referrers, and tag history always go upstream.
- Blobs stream to the caller and a temporary file, then populate the cache only
  after EOF verifies their size and digest. Callers must consume or close readers;
  interrupted downloads are discarded. `CacheOptions.TempDir` selects the temporary
  directory. Manifests are buffered and verified before returning them.
- Concurrent misses for the same repository and digest share one fill. A waiting
  request can cancel independently. Partial range misses forward upstream without
  populating; a full cached blob serves range requests locally.

Only not-found errors trigger fallback. Other cache read errors propagate.
Population and invalidation are best effort: `CacheOptions.OnError` reports failures
without failing a successful upstream operation. There are no background retries
or automatic capacity limits; eviction is the cache backend's responsibility.

### Writes and scope

Writes, upload sessions, and mounts go upstream. Manifest pushes and deletes
invalidate affected cached entries. In-progress fills cannot republish entries
invalidated through the same wrapper. `IfMatch` conditions are evaluated by the
upstream; conditional pushes also perform an authoritative tag lookup for HTTP
ETag discovery.

Invalidation coordination is local to this wrapper instance. Changes made
upstream outside it, or through another instance, do not invalidate its cached
tags; backend expiration or explicit eviction must handle those changes.

Dedicate the cache to one upstream and authorization domain. Apply access-control
middleware outside `Cache` so permissions are checked on cache hits as well.
This package does not add authentication to an upstream client.
