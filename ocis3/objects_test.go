package ocis3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// memoryObjects models conditional object operations for failure/concurrency
// tests. Integration tests exercise the same paths through a real S3 server.
type memoryObjects struct {
	mu               sync.Mutex
	objects          map[string]storedObject
	uploads          map[string]*storedUpload
	failKey          string
	failAfter        bool
	failPrecondition bool
}
type storedObject struct {
	data            []byte
	mediaType, etag string
}
type storedUpload struct {
	key, mediaType string
	parts          map[int32][]byte
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	return &Registry{bucket: "test", prefix: "v1/", partSize: 5 * 1024 * 1024, client: &memoryObjects{objects: map[string]storedObject{}, uploads: map[string]*storedUpload{}}}
}
func apiError(code string) error    { return &smithy.GenericAPIError{Code: code, Message: code} }
func objectETag(data []byte) string { return fmt.Sprintf(`"%x"`, sha256.Sum256(data)) }
func (m *memoryObjects) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aws.ToString(in.Key)
	old, exists := m.objects[key]
	if in.IfNoneMatch != nil && exists || in.IfMatch != nil && (!exists || old.etag != *in.IfMatch) {
		return nil, apiError("PreconditionFailed")
	}
	fail := key == m.failKey
	if fail {
		m.failKey = ""
		if !m.failAfter {
			return nil, fmt.Errorf("injected PUT failure")
		}
	}
	etag := objectETag(data)
	m.objects[key] = storedObject{data: data, etag: etag, mediaType: aws.ToString(in.ContentType)}
	if fail {
		if m.failPrecondition {
			return nil, apiError("PreconditionFailed")
		}
		return nil, fmt.Errorf("injected lost response")
	}
	return &s3.PutObjectOutput{ETag: aws.String(etag)}, nil
}
func (m *memoryObjects) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	obj, ok := m.objects[aws.ToString(in.Key)]
	m.mu.Unlock()
	if !ok {
		return nil, apiError("NoSuchKey")
	}
	data := obj.data
	if in.Range != nil {
		var start, end int
		_, err := fmt.Sscanf(*in.Range, "bytes=%d-%d", &start, &end)
		if err != nil || start < 0 || end >= len(data) || start > end {
			return nil, apiError("InvalidRange")
		}
		data = data[start : end+1]
	}
	return &s3.GetObjectOutput{Body: &contextBody{ReadCloser: io.NopCloser(bytes.NewReader(data)), ctx: ctx}, ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(obj.mediaType), ETag: aws.String(obj.etag)}, nil
}

type contextBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *contextBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}
func (m *memoryObjects) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	out, err := m.GetObject(ctx, &s3.GetObjectInput{Key: in.Key})
	if err != nil {
		return nil, err
	}
	_ = out.Body.Close()
	return &s3.HeadObjectOutput{ContentLength: out.ContentLength, ContentType: out.ContentType, ETag: out.ETag}, nil
}
func (m *memoryObjects) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}
func (m *memoryObjects) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	after := aws.ToString(in.StartAfter)
	if in.ContinuationToken != nil {
		after = *in.ContinuationToken
	}
	keys := []string{}
	for key := range m.objects {
		if strings.HasPrefix(key, aws.ToString(in.Prefix)) && key > after {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(len(keys) > 2)}
	for _, key := range keys[:min(2, len(keys))] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(key)})
	}
	if len(keys) > 2 {
		out.NextContinuationToken = aws.String(keys[1])
	}
	return out, nil
}
func (m *memoryObjects) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := newID()
	m.uploads[id] = &storedUpload{key: aws.ToString(in.Key), mediaType: aws.ToString(in.ContentType), parts: map[int32][]byte{}}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}
func (m *memoryObjects) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[aws.ToString(in.UploadId)]
	if !ok {
		return nil, apiError("NoSuchUpload")
	}
	u.parts[aws.ToInt32(in.PartNumber)] = data
	return &s3.UploadPartOutput{ETag: aws.String(objectETag(data))}, nil
}
func (m *memoryObjects) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := aws.ToString(in.UploadId)
	u, ok := m.uploads[id]
	if !ok {
		return nil, apiError("NoSuchUpload")
	}
	if _, exists := m.objects[u.key]; exists && in.IfNoneMatch != nil {
		return nil, apiError("PreconditionFailed")
	}
	var data []byte
	for i, part := range in.MultipartUpload.Parts {
		p := u.parts[aws.ToInt32(part.PartNumber)]
		if objectETag(p) != aws.ToString(part.ETag) {
			return nil, apiError("InvalidPart")
		}
		if i < len(in.MultipartUpload.Parts)-1 && len(p) < 5*1024*1024 {
			return nil, apiError("EntityTooSmall")
		}
		data = append(data, p...)
	}
	etag := objectETag(data)
	m.objects[u.key] = storedObject{data: data, mediaType: u.mediaType, etag: etag}
	delete(m.uploads, id)
	return &s3.CompleteMultipartUploadOutput{ETag: aws.String(etag)}, nil
}
func (m *memoryObjects) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, aws.ToString(in.UploadId))
	return &s3.AbortMultipartUploadOutput{}, nil
}
