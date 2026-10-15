# ociserver

`ociserver` serves the OCI distribution HTTP API using an `oci.Registry` as its
backend. `Server` implements `http.Handler`; the application supplies storage,
starts the HTTP listener, and manages shutdown and backend cleanup.

## Serve a registry

Pass an initialized `oci.Registry` to `ociserver.New`. A nil configuration uses
the defaults.

```go
package main

import (
    "log"
    "log/slog"
    "net/http"
    "time"

    "github.com/ohseeeye/oci"
    "github.com/ohseeeye/oci/ocimem"
    "github.com/ohseeeye/oci/ociserver"
)

func main() {
    var backend oci.Registry = ocimem.New()
    handler, err := ociserver.New(backend, &ociserver.ServerConfig{
        Logger: slog.Default(),
    })
    if err != nil {
        log.Fatal(err)
    }

    server := &http.Server{
        Addr:              "127.0.0.1:5000",
        Handler:           handler,
        ReadHeaderTimeout: 10 * time.Second,
    }
    log.Fatal(server.ListenAndServe())
}
```

Replace `ocimem` with `ocilayout`, `ocisqlite`, an `ociclient` connected to an
upstream registry, or your own implementation. Registry middleware such as
`ocimiddleware.ReadOnly` can wrap the backend before it is passed to `New`.
Backend options control storage behavior, including sparse manifest acceptance.

`New` does not start a listener or close the backend. Stop serving requests and
finish active operations before closing a backend that owns resources.

### HTTP operations

The built-in routes under `/v2` cover:

- Registry discovery at `GET /v2/`.
- Blob GET, HEAD, byte-range reads, and deletion.
- Blob uploads, resumable upload sessions, cancellation, and cross-repository mounts.
- Manifest GET and HEAD by tag or digest, PUT, and deletion by tag or digest.
- Tag listing with pagination.
- Referrers with optional `artifactType` filtering.

Storage operations are delegated to the supplied backend. Manifest PUT accepts
OCI image manifests and indexes, and Docker schema 2 manifests and manifest lists,
with a 4 MiB request-body limit. There is no built-in repository catalog endpoint.

## Configuration and custom routes

`ServerConfig` exposes:

| Field | Behavior |
|-------|----------|
| `Logger` | A `*slog.Logger` for unexpected internal failures. Nil disables these diagnostics. |
| `Middlewares` | HTTP middleware applied to all routes, including custom routes. The first entry is outermost. |

Registry-owned authentication is supplied through the optional `Authorizer`
interface described below. Use `Middlewares` for request logging, tracing, or
application-wide authentication that should also cover custom routes.

Add application routes outside `/v2` through `Server.Mux()`:

```go
handler.Mux().Get("/health", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusNoContent)
})
```

This route receives `Middlewares`, but not the registry's `Authorizer`. Configure root
middleware in `ServerConfig` before construction; the router does not allow
middleware to be added after routes have been registered. `New` copies the
configuration and its middleware slice.

## Authentication and authorization

If the backend passed to `New` also implements `ociserver.Authorizer`, the server
automatically calls its HTTP authorization hook for matched built-in OCI requests:

```go
type Authorizer interface {
    Authorize(w http.ResponseWriter, r *http.Request, repo, reference string, next http.Handler)
}
```

`Authorize` can parse and validate credentials, attach an identity to the request
context, and call `next.ServeHTTP(w, r.WithContext(ctx))` to continue. Registry
methods receive that context and can enforce permissions using the same
registry's policy. The registry implementation chooses the identity type and
authentication mechanism; `ociserver` does not configure credentials or a token
service.

The hook owns authentication failure responses, including
`WWW-Authenticate` challenges, denials, and browser login redirects. It has access
to the original HTTP request and response writer.

Repository names and references are extracted by the router before `Authorize`
runs. The values have these meanings:

| Request | `repo` | `reference` |
|---------|--------|-------------|
| `/v2/` discovery | Empty | Empty |
| Manifest operations | Repository name | Tag or digest from the path |
| Blob operations and referrers | Repository name | Digest from the path |
| Tag history | Repository name | Tag from the path |
| Tag listing and upload sessions | Repository name | Empty |

These are the routed path values, before the OCI handlers validate them. Tags
are not resolved to digests before authorization. Upload session IDs and query
parameters are not content references; mount source repositories and upload
digests remain available through `r.URL.Query()`.

For example, an application can package its auth hook with the registry. This
snippet uses `net/http`, `oci`, and `ociserver`:

```go
type securedRegistry struct {
    oci.Registry
    authorize func(http.ResponseWriter, *http.Request, string, string, http.Handler)
}

var _ ociserver.Authorizer = (*securedRegistry)(nil)

func (r *securedRegistry) Authorize(w http.ResponseWriter, req *http.Request, repo, reference string, next http.Handler) {
    r.authorize(w, req, repo, reference, next)
}
```

Pass `&securedRegistry{Registry: backend, authorize: authorizeRequest}` to `New`,
where `authorizeRequest` is the application's auth hook. No auth field in
`ServerConfig` is needed. This replaces the former `AuthMiddleware` field.

The hook runs before the OCI handlers, including `/v2/` discovery, uploads,
and optional tag-history routes. `ServerConfig.Middlewares` run outside it.
Custom routes, including login and callback endpoints registered through `Mux`,
do not receive it; protect those separately when needed. A registry without
`Authorizer` supplies no HTTP authentication through this mechanism.

`Authorize` runs once per matched request and must support concurrent requests.
Outer registry wrappers must explicitly preserve `Authorizer` for automatic
detection to work. Direct Go calls do not pass through this HTTP middleware;
a registry that checks identity in its methods must also support an appropriate
context for those callers.

## Blob redirects

If the backend passed to `New` also implements `ociserver.Redirecter`, the server
can redirect blob GET requests to storage-backed URLs, such as signed object
store URLs:

```go
type Redirecter interface {
    Redirect(r *http.Request, desc oci.Descriptor, repo string) (bool, string, error)
}
```

The server resolves the blob through `Registry.ResolveBlob` first, then calls
`Redirect` with its descriptor and repository name.

- Return `true, url, nil` to send HTTP `307 Temporary Redirect`.
- Return `false, "", nil` to stream through `Registry.GetBlob` or `GetBlobRange`.
- Return an error to send HTTP `500`; the server logs it when a logger is configured.

For example, an application can adapt its URL-signing function with a registry
wrapper. This snippet uses `context`, `net/http`, `oci`, and `ociserver`:

```go
type redirectRegistry struct {
    oci.Registry
    blobURL func(context.Context, string, oci.Descriptor) (string, error)
}

var _ ociserver.Redirecter = (*redirectRegistry)(nil)

func (r *redirectRegistry) Redirect(req *http.Request, desc oci.Descriptor, repo string) (bool, string, error) {
    url, err := r.blobURL(req.Context(), repo, desc)
    if err != nil {
        return false, "", err
    }
    return true, url, nil
}
```

Pass `&redirectRegistry{Registry: backend, blobURL: signBlobURL}` to `New`, where
`signBlobURL` is the application's signing function.

Redirects apply only to blob GET, including requests with a `Range` header. The
target must support the client's requested range. Blob HEAD returns metadata
directly; manifests and uploads are not redirected. Setting `Descriptor.URLs`
alone does not enable redirects: this server uses the `Redirecter` interface.

Optional capabilities are detected on the backend value passed to `New`. Any
registry wrappers must explicitly preserve `Redirecter` if redirects should
remain available through them.

## Experimental extensions

### Conditional manifest updates

Manifest GET and HEAD responses by tag or digest include an `ETag` containing
the quoted manifest digest. Manifest PUT also returns this ETag.

A PUT to a single tag can send that value in `If-Match`. The server passes it to
`oci.PushManifestParameters.IfMatch`; supporting backends compare and update the
tag atomically. `ocimem`, `ocilayout`, and `ocisqlite` support this condition.
A mismatch returns HTTP `400` with `MANIFEST_INVALID`. Without the header,
ordinary unconditional pushes continue to work.

The supported condition is a single quoted digest. Weak ETags, lists, and `*`
are not supported. Conditions on digest references or bulk tag assignments are
rejected. Backends without conditional-write support may ignore `IfMatch`, so
an ETag alone does not guarantee that a custom backend enforces the condition.

### Tag history

When the supplied backend implements `oci.TagHistory`, the server registers
`GET /v2/<repository>/_oci/tag-history/<tag>`. It returns an OCI image index of
historical descriptor snapshots and supports `n`, `before`, `since`, and `digest`
query parameters, with `Link` headers for subsequent pages. This endpoint is
absent when the backend does not expose the capability.

Entries carry timestamp and `created` or `deleted` event annotations. The
`before` and `since` bounds are exclusive RFC 3339 timestamps. See the
[tag-history interface](../tag_history.go) for the descriptor annotations and
filter definitions. Registry wrappers must preserve `oci.TagHistory` for the
endpoint to remain available.

## Logging and events

There is currently no built-in event subscription, event callback, or webhook
delivery API. `ServerConfig.Logger` records unexpected failures; it is not an
access log or an event stream. `ocimiddleware.Debug` can log registry operations
for diagnostics.

Applications can wrap HTTP handlers to record requests and responses, or wrap
`oci.Registry` to emit application events after successful operations. To track
completed chunked uploads, also wrap the returned `oci.BlobWriter` and observe
`Commit`; creating or writing an upload session is not a completed blob push.
For redirected downloads, the server can observe issuing the redirect, but not
whether the client finishes downloading from the target.
