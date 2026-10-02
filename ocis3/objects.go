package ocis3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ohseeeye/oci"
)

type objectClient interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// Operations use seekable, bounded buffers so the SDK can retry requests.
func (r *Registry) put(ctx context.Context, key string, data []byte, mediaType, etag string, absent bool) (string, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(mediaType)}
	if absent {
		in.IfNoneMatch = aws.String("*")
	} else if etag != "" {
		in.IfMatch = aws.String(etag)
	}
	out, err := r.client.PutObject(ctx, in)
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ETag), nil
}

func (r *Registry) putJSON(ctx context.Context, key string, value any, etag string, absent bool) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return r.put(ctx, key, data, "application/json", etag, absent)
}

const maxRecordSize = 16 * 1024 * 1024

func (r *Registry) getJSON(ctx context.Context, key string, value any) (string, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxRecordSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxRecordSize {
		return "", fmt.Errorf("object record exceeds size limit")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return "", fmt.Errorf("invalid object record: %w", err)
	}
	etag := aws.ToString(out.ETag)
	if etag == "" {
		return "", fmt.Errorf("object store did not return an ETag")
	}
	return etag, nil
}

func (r *Registry) head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	return r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
}
func (r *Registry) remove(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	return err
}

func (r *Registry) list(ctx context.Context, prefix, after string, visit func(string) (bool, error)) error {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix)}
	if after != "" {
		in.StartAfter = aws.String(after)
	}
	for {
		out, err := r.client.ListObjectsV2(ctx, in)
		if err != nil {
			return err
		}
		for _, item := range out.Contents {
			key := aws.ToString(item.Key)
			if !strings.HasPrefix(key, prefix) {
				return fmt.Errorf("object store returned a key outside the requested prefix")
			}
			more, err := visit(key)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		if aws.ToString(out.NextContinuationToken) == "" || aws.ToString(out.NextContinuationToken) == aws.ToString(in.ContinuationToken) {
			return fmt.Errorf("invalid listing continuation token")
		}
		in.ContinuationToken = out.NextContinuationToken
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type blobReader struct {
	io.ReadCloser
	desc oci.Descriptor
}

func (r *blobReader) Descriptor() oci.Descriptor { return r.desc }
