package ocis3

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ohseeeye/oci"
)

type uploadChunk struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}
type uploadSession struct {
	Revision string        `json:"revision"`
	Status   string        `json:"status"`
	Size     int64         `json:"size"`
	Chunks   []uploadChunk `json:"chunks"`
	Digest   oci.Digest    `json:"digest,omitempty"`
}

// PushBlobChunked starts a durable session made of immutable chunk objects.
func (r *Registry) PushBlobChunked(ctx context.Context, repo string, chunkSize int) (oci.BlobWriter, error) {
	return r.PushBlobChunkedResume(ctx, repo, "", 0, chunkSize)
}

// PushBlobChunkedResume resumes an exact offset or the last committed offset (-1).
func (r *Registry) PushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (oci.BlobWriter, error) {
	if err := validateRepo(repo); err != nil {
		return nil, err
	}
	if offset < -1 {
		return nil, oci.ErrRangeInvalid
	}
	var state uploadSession
	var etag string
	var err error
	if id == "" {
		if offset > 0 {
			return nil, oci.ErrRangeInvalid
		}
		if err := r.ensureRepo(ctx, repo); err != nil {
			return nil, err
		}
		id = newID()
		state = uploadSession{Revision: newID(), Status: "active"}
		etag, err = r.putJSON(ctx, r.repoKey(repo, "_uploads/"+id+"/session"), state, "", true)
	} else {
		if !validID(id) {
			return nil, oci.ErrBlobUploadUnknown
		}
		etag, err = r.getJSON(ctx, r.repoKey(repo, "_uploads/"+id+"/session"), &state)
		if missing(err) {
			return nil, oci.ErrBlobUploadUnknown
		}
	}
	if err != nil {
		return nil, err
	}
	if state.Status != "active" && state.Status != "committing" {
		return nil, oci.ErrBlobUploadUnknown
	}
	if offset >= 0 && offset != state.Size {
		return nil, oci.ErrRangeInvalid
	}
	if chunkSize <= 0 {
		chunkSize = r.partSize
	}
	if chunkSize > r.partSize {
		chunkSize = r.partSize
	}
	return &blobWriter{registry: r, ctx: ctx, repo: repo, id: id, etag: etag, state: state, chunkSize: chunkSize}, nil
}

type blobWriter struct {
	mu             sync.Mutex
	registry       *Registry
	ctx            context.Context
	repo, id, etag string
	state          uploadSession
	chunkSize      int
	closed         bool
}

func (w *blobWriter) Size() int64    { w.mu.Lock(); defer w.mu.Unlock(); return w.state.Size }
func (w *blobWriter) ID() string     { return w.id }
func (w *blobWriter) ChunkSize() int { return w.chunkSize }
func (w *blobWriter) key() string    { return w.registry.repoKey(w.repo, "_uploads/"+w.id+"/session") }

func (w *blobWriter) publish(ctx context.Context, next uploadSession) error {
	next.Revision = newID()
	etag, err := w.registry.putJSON(ctx, w.key(), next, w.etag, false)
	if err != nil {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var actual uploadSession
		actualETag, checkErr := w.registry.getJSON(checkCtx, w.key(), &actual)
		if checkErr != nil || actual.Revision != next.Revision {
			if conflict(err) {
				return oci.ErrRangeInvalid
			}
			return err
		}
		etag = actualETag
	}
	w.state, w.etag = next, etag
	return nil
}

func (w *blobWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.state.Status != "active" {
		return 0, fmt.Errorf("upload is not writable")
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	next := w.state
	next.Chunks = slices.Clone(next.Chunks)
	for len(p) > 0 {
		if len(next.Chunks) >= 10000 {
			return 0, fmt.Errorf("upload exceeds chunk capacity")
		}
		n := min(len(p), w.chunkSize)
		key := w.registry.repoKey(w.repo, "_uploads/"+w.id+"/chunks/"+newID())
		if _, err := w.registry.put(w.ctx, key, p[:n], "application/octet-stream", "", true); err != nil {
			return 0, err
		}
		next.Chunks = append(next.Chunks, uploadChunk{Key: key, Size: int64(n)})
		next.Size += int64(n)
		p = p[n:]
	}
	if next.Size == w.state.Size {
		return 0, nil
	}
	n := next.Size - w.state.Size
	if err := w.publish(w.ctx, next); err != nil {
		return 0, err
	}
	return int(n), nil
}
func (w *blobWriter) Close() error { w.mu.Lock(); defer w.mu.Unlock(); w.closed = true; return nil }

func (w *blobWriter) Commit(digest oci.Digest) (oci.Descriptor, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := digest.Validate(); err != nil {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	if w.state.Status == "complete" {
		if digest != w.state.Digest {
			return oci.Descriptor{}, oci.ErrDigestInvalid
		}
		return oci.Descriptor{Digest: digest, Size: w.state.Size, MediaType: "application/octet-stream"}, nil
	}
	if w.state.Status != "active" && w.state.Status != "committing" {
		return oci.Descriptor{}, oci.ErrBlobUploadUnknown
	}
	if w.state.Status == "active" {
		next := w.state
		next.Status = "committing"
		next.Digest = digest
		if err := w.publish(w.ctx, next); err != nil {
			return oci.Descriptor{}, err
		}
	} else if w.state.Digest != digest {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	reader := &chunkReader{registry: w.registry, ctx: w.ctx, chunks: w.state.Chunks, prefix: w.registry.repoKey(w.repo, "_uploads/"+w.id+"/chunks/")}
	defer reader.Close()
	desc, err := w.registry.PushBlob(w.ctx, w.repo, oci.Descriptor{Digest: digest, Size: w.state.Size}, reader)
	if err != nil {
		// A bad digest must not permanently freeze an otherwise resumable upload.
		if err == oci.ErrDigestInvalid || err == oci.ErrSizeInvalid {
			next := w.state
			next.Status = "active"
			next.Digest = ""
			if resetErr := w.publish(w.ctx, next); resetErr != nil {
				return oci.Descriptor{}, fmt.Errorf("%w (reset upload: %v)", err, resetErr)
			}
		}
		return oci.Descriptor{}, err
	}
	next := w.state
	next.Status = "complete"
	if err := w.publish(w.ctx, next); err != nil {
		return oci.Descriptor{}, err
	}
	w.closed = true
	w.cleanup(w.ctx)
	return desc, nil
}

func (w *blobWriter) cleanup(ctx context.Context) {
	for _, chunk := range w.state.Chunks {
		_ = w.registry.remove(ctx, chunk.Key)
	}
}

func (w *blobWriter) Cancel() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Status == "complete" || w.state.Status == "canceled" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 30*time.Second)
	defer cancel()
	var current uploadSession
	etag, err := w.registry.getJSON(ctx, w.key(), &current)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	w.state, w.etag = current, etag
	if current.Status == "complete" || current.Status == "canceled" {
		return nil
	}
	// A committing upload has durable intent and may be retried by another
	// process; cancellation cannot revoke content already being published.
	if current.Status == "committing" {
		return fmt.Errorf("%w: upload commit in progress", oci.ErrDenied)
	}
	next := current
	next.Status = "canceled"
	if err := w.publish(ctx, next); err != nil {
		return err
	}
	w.closed = true
	w.cleanup(ctx)
	return nil
}

type chunkReader struct {
	registry  *Registry
	ctx       context.Context
	chunks    []uploadChunk
	prefix    string
	body      io.ReadCloser
	remaining int64
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	for {
		if c.body == nil {
			if len(c.chunks) == 0 {
				return 0, io.EOF
			}
			chunk := c.chunks[0]
			c.chunks = c.chunks[1:]
			if len(chunk.Key) <= len(c.prefix) || chunk.Key[:len(c.prefix)] != c.prefix || !validID(chunk.Key[len(c.prefix):]) || chunk.Size < 0 {
				return 0, fmt.Errorf("invalid upload chunk")
			}
			out, err := c.registry.client.GetObject(c.ctx, &s3.GetObjectInput{Bucket: aws.String(c.registry.bucket), Key: aws.String(chunk.Key)})
			if err != nil {
				return 0, err
			}
			c.body = out.Body
			c.remaining = chunk.Size
			if aws.ToInt64(out.ContentLength) != chunk.Size {
				_ = c.Close()
				return 0, fmt.Errorf("upload chunk size mismatch")
			}
		}
		n, err := c.body.Read(p)
		c.remaining -= int64(n)
		if c.remaining < 0 {
			_ = c.Close()
			return n, fmt.Errorf("upload chunk grew")
		}
		if err == io.EOF {
			_ = c.Close()
			if c.remaining != 0 {
				return n, io.ErrUnexpectedEOF
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
func (c *chunkReader) Close() error {
	if c.body == nil {
		return nil
	}
	err := c.body.Close()
	c.body = nil
	return err
}
