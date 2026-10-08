// Package ocimiddleware wraps OCI registries with access controls, restricted
// views, repository routing, read-through caching, and operation logging.
//
// Each wrapper accepts an existing oci.Registry and returns a registry that
// can be passed to another wrapper or served by ociserver.
package ocimiddleware
