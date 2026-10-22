package ocis3

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// ResolveBlob describes content available through the named repository.
func (r *Registry) ResolveBlob(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	if err := r.checkRepo(ctx, repo); err != nil {
		return oci.Descriptor{}, err
	}
	membership, err := r.membershipKey(repo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := r.head(ctx, membership); err != nil {
		if missing(err) {
			return oci.Descriptor{}, oci.ErrBlobUnknown
		}
		return oci.Descriptor{}, err
	}
	key, err := r.blobKey(digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	out, err := r.head(ctx, key)
	if err != nil {
		return oci.Descriptor{}, err
	}
	return oci.Descriptor{Digest: digest, Size: aws.ToInt64(out.ContentLength), MediaType: "application/octet-stream"}, nil
}

// ResolveManifest describes a repository's immutable manifest object.
func (r *Registry) ResolveManifest(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	if err := r.checkRepo(ctx, repo); err != nil {
		return oci.Descriptor{}, err
	}
	key, err := r.manifestKey(repo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	out, err := r.head(ctx, key)
	if err != nil {
		if missing(err) {
			return oci.Descriptor{}, oci.ErrManifestUnknown
		}
		return oci.Descriptor{}, err
	}
	return oci.Descriptor{Digest: digest, Size: aws.ToInt64(out.ContentLength), MediaType: aws.ToString(out.ContentType)}, nil
}

// ResolveTag resolves the current event pointer. Deleted pointers retain history.
func (r *Registry) ResolveTag(ctx context.Context, repo, tag string) (oci.Descriptor, error) {
	if !ociref.IsValidTag(tag) {
		return oci.Descriptor{}, oci.ErrNameInvalid
	}
	if err := r.checkRepo(ctx, repo); err != nil {
		return oci.Descriptor{}, err
	}
	pointer, _, err := r.tagPointer(ctx, repo, tag)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if pointer.Event.Event != oci.TagHistoryEventCreated {
		return oci.Descriptor{}, oci.ErrManifestUnknown
	}
	// Reject dangling pointers left by a concurrent manifest deletion.
	return r.ResolveManifest(ctx, repo, pointer.Event.Descriptor.Digest)
}

// GetBlob opens a repository blob.
func (r *Registry) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	return r.GetBlobRange(ctx, repo, digest, 0, -1)
}

// GetBlobRange opens a half-open byte range; a negative end selects the remainder.
func (r *Registry) GetBlobRange(ctx context.Context, repo string, digest oci.Digest, start, end int64) (oci.BlobReader, error) {
	desc, err := r.ResolveBlob(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	if end < 0 || end > desc.Size {
		end = desc.Size
	}
	if start < 0 || start > end {
		return nil, oci.ErrRangeInvalid
	}
	if start == end {
		return &blobReader{ReadCloser: io.NopCloser(bytes.NewReader(nil)), desc: desc}, nil
	}
	key, err := r.blobKey(digest)
	if err != nil {
		return nil, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)}
	if start != 0 || end != desc.Size {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", start, end-1))
	}
	out, err := r.client.GetObject(ctx, in)
	if err != nil {
		return nil, err
	}
	if aws.ToInt64(out.ContentLength) != end-start {
		_ = out.Body.Close()
		return nil, fmt.Errorf("object store returned unexpected range length")
	}
	return &blobReader{ReadCloser: &contextReadCloser{ReadCloser: out.Body, ctx: ctx}, desc: desc}, nil
}

// GetManifest opens immutable manifest bytes.
func (r *Registry) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	desc, err := r.ResolveManifest(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	key, err := r.manifestKey(repo, digest)
	if err != nil {
		return nil, err
	}
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return &blobReader{ReadCloser: &contextReadCloser{ReadCloser: out.Body, ctx: ctx}, desc: desc}, nil
}

type contextReadCloser struct {
	io.ReadCloser
	ctx context.Context
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}

// GetTag opens the manifest selected by the tag pointer.
func (r *Registry) GetTag(ctx context.Context, repo, tag string) (oci.BlobReader, error) {
	desc, err := r.ResolveTag(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	return r.GetManifest(ctx, repo, desc.Digest)
}
