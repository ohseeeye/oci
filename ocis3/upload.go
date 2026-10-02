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
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

type uploadChunk struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
}
type uploadSession struct {
	Revision    string          `json:"revision"`
	Status      string          `json:"status"`
	Size        int64           `json:"size"`
	Chunks      []uploadChunk   `json:"chunks"`
	Digest      oci.Digest      `json:"digest,omitempty"`
	DigestState ocidigest.State `json:"digest_state"`
}

// digester restores exactly the algorithms maintained by new uploads. Digest
// offsets and the recorded chunk sizes must describe the same committed bytes.
func (s uploadSession) digester() (*ocidigest.Writer, error) {
	dw, err := ocidigest.NewWriterFromState(nil, s.DigestState)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid upload digest state: %v", oci.ErrBlobUploadInvalid, err)
	}
	algs := dw.Algorithms()
	if len(algs) != 2 || !slices.Contains(algs, ocidigest.SHA256) || !slices.Contains(algs, ocidigest.SHA512) {
		return nil, fmt.Errorf("%w: upload must track SHA-256 and SHA-512", oci.ErrBlobUploadInvalid)
	}
	if s.Size < 0 || dw.Size() != s.Size || len(s.Chunks) > 10000 {
		return nil, fmt.Errorf("%w: upload digest offset mismatch", oci.ErrBlobUploadInvalid)
	}
	var size int64
	for _, chunk := range s.Chunks {
		if chunk.Size <= 0 || chunk.Size > s.Size-size || chunk.ETag == "" {
			return nil, fmt.Errorf("%w: invalid upload chunk record", oci.ErrBlobUploadInvalid)
		}
		size += chunk.Size
	}
	if size != s.Size {
		return nil, fmt.Errorf("%w: upload chunk offset mismatch", oci.ErrBlobUploadInvalid)
	}
	return dw, nil
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
		dw, hashErr := ocidigest.NewWriter(nil, ocidigest.SHA256, ocidigest.SHA512)
		if hashErr != nil {
			return nil, hashErr
		}
		state.DigestState, err = dw.State()
		if err != nil {
			return nil, err
		}
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
	if _, err := state.digester(); err != nil {
		return nil, err
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
	// Restore a separate hasher for this write. Failed chunk uploads or a failed
	// session CAS discard its advanced state without changing the writer's offset.
	dw, err := w.state.digester()
	if err != nil {
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
		etag, err := w.registry.put(w.ctx, key, p[:n], "application/octet-stream", "", true)
		if err != nil {
			return 0, err
		}
		if etag == "" {
			return 0, fmt.Errorf("object store did not return a chunk ETag")
		}
		if _, err := dw.Write(p[:n]); err != nil {
			return 0, err
		}
		next.Chunks = append(next.Chunks, uploadChunk{Key: key, Size: int64(n), ETag: etag})
		next.Size += int64(n)
		p = p[n:]
	}
	if next.Size == w.state.Size {
		return 0, nil
	}
	n := next.Size - w.state.Size
	next.DigestState, err = dw.State()
	if err != nil {
		return 0, err
	}
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
	if err := w.ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	dw, err := w.state.digester()
	if err != nil {
		return oci.Descriptor{}, err
	}
	actual, err := dw.DigestFor(digest.Algorithm())
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("%w: upload digest algorithm is not tracked", oci.ErrDigestInvalid)
	}
	if actual != digest {
		return oci.Descriptor{}, oci.ErrDigestInvalid
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
	// The persisted state already verifies these bytes. Conditional chunk GETs
	// enforce object identity during assembly, so no second hash pass is needed.
	desc, err := w.registry.storeBlob(w.ctx, w.repo, oci.Descriptor{Digest: digest, Size: w.state.Size}, reader, nil)
	if err != nil {
		if err == oci.ErrSizeInvalid {
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
			if len(chunk.Key) <= len(c.prefix) || chunk.Key[:len(c.prefix)] != c.prefix || !validID(chunk.Key[len(c.prefix):]) || chunk.Size <= 0 || chunk.ETag == "" {
				return 0, fmt.Errorf("invalid upload chunk")
			}
			out, err := c.registry.client.GetObject(c.ctx, &s3.GetObjectInput{Bucket: aws.String(c.registry.bucket), Key: aws.String(chunk.Key), IfMatch: aws.String(chunk.ETag)})
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
