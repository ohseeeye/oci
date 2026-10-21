// Copyright 2023 CUE Labs AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ociclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

// GetBlob returns the content of the blob with the given digest.
func (c *Client) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	req, err := newRequest(ctx, http.MethodGet, blobURL(repo, digest), nil, pullScope(repo))
	if err != nil {
		return nil, err
	}
	return c.read(req, digest, false)
}

// GetBlobRange is like GetBlob but asks for only the given byte range.
// If the registry ignores Range and returns the full blob, it discards the
// prefix and limits the returned stream locally. This may transfer extra data,
// but does not buffer the whole blob. The descriptor describes the full blob;
// partial reads do not verify its digest.
func (c *Client) GetBlobRange(ctx context.Context, repo string, digest oci.Digest, o0, o1 int64) (_ oci.BlobReader, _err error) {
	if o0 < 0 || (o1 >= 0 && o1 < o0) {
		return nil, fmt.Errorf("invalid blob range [%d, %d): %w", o0, o1, oci.ErrRangeInvalid)
	}
	if o0 == 0 && o1 < 0 {
		return c.GetBlob(ctx, repo, digest)
	}
	req, err := newRequest(ctx, http.MethodGet, blobURL(repo, digest), nil, pullScope(repo))
	if err != nil {
		return nil, err
	}
	if o1 < 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", o0))
	} else if o1 > o0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", o0, o1-1))
	}
	// Empty ranges cannot be expressed with an HTTP Range header. A full
	// response provides the descriptor and is limited to zero bytes below.
	resp, err := c.do(req, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	defer closeOnError(&_err, resp.Body)
	desc, err := descriptorFromResponse(resp, digest, requireSize)
	if err != nil {
		return nil, fmt.Errorf("invalid descriptor in response: %v", err)
	}
	body := resp.Body
	if resp.StatusCode == http.StatusOK {
		end := desc.Size
		if o1 >= 0 && o1 < end {
			end = o1
		}
		if o0 > end {
			return nil, fmt.Errorf("blob range starts at %d beyond size %d: %w", o0, desc.Size, oci.ErrRangeInvalid)
		}
		if end > o0 {
			if _, err := io.CopyN(io.Discard, resp.Body, o0); err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return nil, fmt.Errorf("cannot skip blob prefix: %w", err)
			}
		}
		body = &blobRangeReader{
			LimitedReader: io.LimitedReader{R: resp.Body, N: end - o0},
			Closer:        resp.Body,
		}
	}
	return newBlobReaderUnverified(body, desc), nil
}

// blobRangeReader limits a full HTTP response while retaining ownership of its
// body. A short response must not look like a successful end of the range.
type blobRangeReader struct {
	io.LimitedReader
	io.Closer
}

func (r *blobRangeReader) Read(p []byte) (int, error) {
	n, err := r.LimitedReader.Read(p)
	if err == io.EOF && r.N > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// ResolveBlob returns the descriptor for the blob with the given digest.
func (c *Client) ResolveBlob(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	req, err := newRequest(ctx, http.MethodHead, blobURL(repo, digest), nil, pullScope(repo))
	if err != nil {
		return oci.Descriptor{}, err
	}
	return c.resolve(req, digest)
}

// ResolveManifest returns the descriptor for the manifest with the given digest.
func (c *Client) ResolveManifest(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	req, err := newManifestRequest(ctx, http.MethodHead, repo, digest.String(), pullScope(repo))
	if err != nil {
		return oci.Descriptor{}, err
	}
	return c.resolve(req, digest)
}

// ResolveTag returns the descriptor for the manifest with the given tag.
func (c *Client) ResolveTag(ctx context.Context, repo string, tag string) (oci.Descriptor, error) {
	req, err := newManifestRequest(ctx, http.MethodHead, repo, tag, pullScope(repo))
	if err != nil {
		return oci.Descriptor{}, err
	}
	return c.resolve(req, "")
}

func (c *Client) resolve(req *http.Request, knownDigest oci.Digest) (oci.Descriptor, error) {
	resp, err := c.do(req)
	if err != nil {
		return oci.Descriptor{}, err
	}
	resp.Body.Close()
	desc, err := descriptorFromResponse(resp, knownDigest, requireSize|requireDigest)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("invalid descriptor in response: %v", err)
	}
	if knownDigest == "" {
		c.observeManifestETag(resp, desc.Digest)
	}
	return desc, nil
}

// GetManifest returns the content of the manifest with the given digest.
func (c *Client) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	req, err := newManifestRequest(ctx, http.MethodGet, repo, digest.String(), pullScope(repo))
	if err != nil {
		return nil, err
	}
	return c.read(req, digest, true)
}

// GetTag returns the content of the manifest with the given tag.
func (c *Client) GetTag(ctx context.Context, repo string, tagName string) (oci.BlobReader, error) {
	req, err := newManifestRequest(ctx, http.MethodGet, repo, tagName, pullScope(repo))
	if err != nil {
		return nil, err
	}
	return c.read(req, "", true)
}

const maxManifestSize = 4 * 1024 * 1024

func (c *Client) read(req *http.Request, knownDigest oci.Digest, isManifest bool) (_ oci.BlobReader, _err error) {
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer closeOnError(&_err, resp.Body)
	desc, err := descriptorFromResponse(resp, knownDigest, requireSize)
	if err != nil {
		return nil, fmt.Errorf("invalid descriptor in response: %v", err)
	}
	if resp.Header.Get("Content-Type") == mediaTypeSchema1SignedManifest {
		// Legacy signed Docker schema1 manifest: strip the libtrust
		// signature envelope here, once, so that the verification
		// below (and every caller of this method) only ever sees a
		// plain schema1 manifest whose bytes hash to desc.Digest,
		// exactly like any other media type. See schema1Unsign.
		if desc.Size > maxManifestSize {
			return nil, fmt.Errorf("manifest size %d exceeds maximum size %d", desc.Size, maxManifestSize)
		}
		signed, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
		if err != nil {
			return nil, fmt.Errorf("failed to read signed schema1 manifest: %v", err)
		}
		resp.Body.Close()
		if int64(len(signed)) != desc.Size {
			return nil, fmt.Errorf("signed schema1 manifest size mismatch")
		}
		unsigned, err := schema1Unsign(signed)
		if err != nil {
			return nil, fmt.Errorf("cannot unsign schema1 manifest: %w", err)
		}
		desc.MediaType = mediaTypeSchema1Manifest
		desc.Size = int64(len(unsigned))
		resp.Body = io.NopCloser(bytes.NewReader(unsigned))
	}
	if desc.Digest == "" {
		// Returning a digest isn't mandatory according to the spec, and
		// at least one registry (AWS's ECR) fails to return a digest
		// when doing a GET of a tag.
		// We know the request must be a tag-getting
		// request because all other requests take a digest not a tag
		// but sanity check anyway.
		if !isManifest {
			return nil, fmt.Errorf("internal error: no digest available for non-tag request")
		}

		if desc.Size > maxManifestSize {
			return nil, fmt.Errorf("manifest size %d exceeds maximum size %d", desc.Size, maxManifestSize)
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
		if err != nil {
			return nil, fmt.Errorf("failed to read body to determine digest: %v", err)
		}
		if int64(len(data)) != desc.Size {
			return nil, fmt.Errorf("body size mismatch")
		}
		desc.Digest = ocidigest.FromBytes(data)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	if isManifest && knownDigest == "" {
		c.observeManifestETag(resp, desc.Digest)
	}
	return newBlobReader(resp.Body, desc), nil
}

func blobURL(repo string, digest oci.Digest) string {
	return "/v2/" + repo + "/blobs/" + digest.String()
}

func manifestURL(repo string, tagOrDigest string) string {
	return "/v2/" + repo + "/manifests/" + tagOrDigest
}
