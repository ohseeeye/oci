// Package ocis3 implements an OCI registry using only S3 objects. Content,
// repository membership, tags, referrers, upload sessions, and tag history are
// stored under versioned keys, without a database or aggregate index file.
package ocis3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// Options configures the registry's object namespace and multipart transfers.
type Options struct {
	// Prefix is a bucket-relative namespace; the registry adds v1/ beneath it.
	Prefix string
	// PartSize defaults to 8 MiB and must be between 5 MiB and 64 MiB.
	PartSize int
}

// Registry implements [oci.Registry] and the experimental [oci.TagHistory].
// The caller owns the S3 client and bucket, which must already exist.
type Registry struct {
	*oci.Funcs
	client         objectClient
	bucket, prefix string
	partSize       int
}

var _ oci.Registry = (*Registry)(nil)
var _ oci.TagHistory = (*Registry)(nil)

// New constructs a registry. The client configures credentials, region,
// endpoint, and path-style addressing. S3 must support conditional PUTs,
// conditional multipart completion, range GETs, and consistent object listing.
func New(client *s3.Client, bucket string, opts *Options) (*Registry, error) {
	if client == nil {
		return nil, fmt.Errorf("S3 client must not be nil")
	}
	if bucket == "" {
		return nil, fmt.Errorf("bucket must not be empty")
	}
	var o Options
	if opts != nil {
		o = *opts
	}
	o.Prefix = strings.Trim(o.Prefix, "/")
	if strings.ContainsAny(o.Prefix, "\\\x00") {
		return nil, fmt.Errorf("invalid object prefix")
	}
	for _, component := range strings.Split(o.Prefix, "/") {
		if component == "." || component == ".." {
			return nil, fmt.Errorf("invalid object prefix")
		}
	}
	if o.PartSize == 0 {
		o.PartSize = 8 * 1024 * 1024
	}
	if o.PartSize < 5*1024*1024 || o.PartSize > 64*1024*1024 {
		return nil, fmt.Errorf("part size must be between 5 MiB and 64 MiB")
	}
	prefix := "v1/"
	if o.Prefix != "" {
		prefix = o.Prefix + "/" + prefix
	}
	return &Registry{client: client, bucket: bucket, prefix: prefix, partSize: o.PartSize}, nil
}

func validateRepo(repo string) error {
	if !ociref.IsValidRepository(repo) || len(repo) > 255 {
		return oci.ErrNameInvalid
	}
	return nil
}

func digestKey(digest oci.Digest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", oci.ErrDigestInvalid, err)
	}
	return digest.Algorithm().String() + "/" + digest.Encoded(), nil
}

func (r *Registry) repoKey(repo, suffix string) string {
	return r.prefix + "repositories/" + repo + "/" + suffix
}
func (r *Registry) blobKey(digest oci.Digest) (string, error) {
	key, err := digestKey(digest)
	return r.prefix + "blobs/" + key, err
}
func (r *Registry) manifestKey(repo string, digest oci.Digest) (string, error) {
	key, err := digestKey(digest)
	return r.repoKey(repo, "_manifests/"+key), err
}
func (r *Registry) membershipKey(repo string, digest oci.Digest) (string, error) {
	key, err := digestKey(digest)
	return r.repoKey(repo, "_blobs/"+key), err
}

func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}
func missing(err error) bool {
	switch errorCode(err) {
	case "NoSuchKey", "NotFound", "404":
		return true
	}
	return false
}
func conflict(err error) bool {
	switch errorCode(err) {
	case "PreconditionFailed", "ConditionalRequestConflict", "412", "409":
		return true
	}
	return false
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
}

func (r *Registry) ensureRepo(ctx context.Context, repo string) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	_, err := r.put(ctx, r.prefix+"catalog/"+repo, nil, "application/octet-stream", "", true)
	if conflict(err) {
		return nil
	}
	return err
}
func (r *Registry) checkRepo(ctx context.Context, repo string) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	_, err := r.head(ctx, r.prefix+"catalog/"+repo)
	if missing(err) {
		return oci.ErrNameUnknown
	}
	return err
}
