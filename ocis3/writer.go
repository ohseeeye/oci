package ocis3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// PushBlob verifies the expected size and digest before completing a multipart
// object. Repository membership is published only after content is committed.
func (r *Registry) PushBlob(ctx context.Context, repo string, desc oci.Descriptor, content io.Reader) (oci.Descriptor, error) {
	if err := validateRepo(repo); err != nil {
		return oci.Descriptor{}, err
	}
	if desc.Size < 0 {
		return oci.Descriptor{}, oci.ErrSizeInvalid
	}
	key, err := r.blobKey(desc.Digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	dw, err := ocidigest.NewWriter(nil, desc.Digest.Algorithm())
	if err != nil {
		return oci.Descriptor{}, err
	}
	buf := make([]byte, r.partSize)
	var uploadID string
	var parts []types.CompletedPart
	defer func() {
		if uploadID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, _ = r.client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID)})
		}
	}()
	source := contextReader{ctx: ctx, reader: content}
	for {
		n, readErr := io.ReadFull(source, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return oci.Descriptor{}, readErr
		}
		if n > 0 {
			if _, err := dw.Write(buf[:n]); err != nil {
				return oci.Descriptor{}, err
			}
			if dw.Size() > desc.Size {
				return oci.Descriptor{}, oci.ErrSizeInvalid
			}
			if uploadID == "" {
				out, err := r.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(key), ContentType: aws.String("application/octet-stream")})
				if err != nil {
					return oci.Descriptor{}, err
				}
				uploadID = aws.ToString(out.UploadId)
				if uploadID == "" {
					return oci.Descriptor{}, fmt.Errorf("missing multipart upload ID")
				}
			}
			if len(parts) >= 10000 {
				return oci.Descriptor{}, fmt.Errorf("blob exceeds configured multipart capacity")
			}
			number := int32(len(parts) + 1)
			out, err := r.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(number), ContentLength: aws.Int64(int64(n)), Body: bytes.NewReader(buf[:n])})
			if err != nil {
				return oci.Descriptor{}, err
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: out.ETag})
		}
		if readErr != nil {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if dw.Size() != desc.Size {
		return oci.Descriptor{}, oci.ErrSizeInvalid
	}
	digest, err := dw.Digest()
	if err != nil {
		return oci.Descriptor{}, err
	}
	if digest != desc.Digest {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	if uploadID == "" {
		_, err = r.put(ctx, key, nil, "application/octet-stream", "", true)
	} else {
		_, err = r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, IfNoneMatch: aws.String("*")})
		if err == nil {
			uploadID = ""
		}
	}
	if conflict(err) {
		out, headErr := r.head(ctx, key)
		if headErr != nil {
			return oci.Descriptor{}, headErr
		}
		if aws.ToInt64(out.ContentLength) != desc.Size {
			return oci.Descriptor{}, oci.ErrSizeInvalid
		}
	} else if err != nil {
		return oci.Descriptor{}, err
	}
	if err := r.ensureRepo(ctx, repo); err != nil {
		return oci.Descriptor{}, err
	}
	membership, err := r.membershipKey(repo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := r.put(ctx, membership, nil, "application/octet-stream", "", true); err != nil && !conflict(err) {
		return oci.Descriptor{}, err
	}
	return oci.Descriptor{Digest: digest, Size: desc.Size, MediaType: "application/octet-stream"}, nil
}

// MountBlob publishes membership without copying content.
func (r *Registry) MountBlob(ctx context.Context, fromRepo, toRepo string, digest oci.Digest) (oci.Descriptor, error) {
	desc, err := r.ResolveBlob(ctx, fromRepo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if err := r.ensureRepo(ctx, toRepo); err != nil {
		return oci.Descriptor{}, err
	}
	key, err := r.membershipKey(toRepo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := r.put(ctx, key, nil, "application/octet-stream", "", true); err != nil && !conflict(err) {
		return oci.Descriptor{}, err
	}
	return desc, nil
}

// PushManifest publishes a repository manifest, referrer entry, and each tag.
// Objects are committed separately; multiple tags are not updated atomically.
func (r *Registry) PushManifest(ctx context.Context, repo string, data []byte, mediaType string, params *oci.PushManifestParameters) (oci.Descriptor, error) {
	if err := validateRepo(repo); err != nil {
		return oci.Descriptor{}, err
	}
	if mediaType == "" || len(mediaType) > oci.MaxMediaTypeLen {
		return oci.Descriptor{}, oci.ErrManifestInvalid
	}
	if len(data) > 4*1024*1024 {
		return oci.Descriptor{}, oci.ErrManifestInvalid
	}
	data = slices.Clone(data)
	digest := ocidigest.FromBytes(data)
	var tags []string
	if params != nil {
		if params.Digest != "" {
			digest = params.Digest
		}
		tags = slices.Compact(slices.Sorted(slices.Values(params.Tags)))
	}
	for _, tag := range tags {
		if !ociref.IsValidTag(tag) {
			return oci.Descriptor{}, oci.ErrNameInvalid
		}
	}
	if err := digest.Validate(); err != nil {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	dw, err := ocidigest.NewWriter(nil, digest.Algorithm())
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := dw.Write(data); err != nil {
		return oci.Descriptor{}, err
	}
	got, err := dw.Digest()
	if err != nil {
		return oci.Descriptor{}, err
	}
	if got != digest {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	info, err := manifestInfoFromBytes(mediaType, data)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("%w: %v", oci.ErrManifestInvalid, err)
	}
	for _, child := range info.children {
		if !child.manifest && len(child.desc.URLs) > 0 {
			continue
		}
		var desc oci.Descriptor
		if child.manifest {
			desc, err = r.ResolveManifest(ctx, repo, child.desc.Digest)
		} else {
			desc, err = r.ResolveBlob(ctx, repo, child.desc.Digest)
		}
		if err != nil {
			return oci.Descriptor{}, fmt.Errorf("%w: missing child %s: %v", oci.ErrManifestInvalid, child.desc.Digest, err)
		}
		if desc.Size != child.desc.Size || (child.manifest && desc.MediaType != child.desc.MediaType) {
			return oci.Descriptor{}, fmt.Errorf("%w: child descriptor mismatch", oci.ErrManifestInvalid)
		}
	}
	if err := r.ensureRepo(ctx, repo); err != nil {
		return oci.Descriptor{}, err
	}
	key, err := r.manifestKey(repo, digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := r.put(ctx, key, data, mediaType, "", true); err != nil {
		if !conflict(err) {
			return oci.Descriptor{}, err
		}
		existing, err := r.ResolveManifest(ctx, repo, digest)
		if err != nil {
			return oci.Descriptor{}, err
		}
		if existing.MediaType != mediaType {
			return oci.Descriptor{}, fmt.Errorf("%w: manifest media type mismatch", oci.ErrDenied)
		}
	}
	desc := oci.Descriptor{Digest: digest, MediaType: mediaType, Size: int64(len(data))}
	if info.subject != "" {
		refKey, err := r.referrerKey(repo, info.subject, digest)
		if err != nil {
			return oci.Descriptor{}, err
		}
		ref := desc
		ref.ArtifactType, ref.Annotations = info.artifactType, info.annotations
		if _, err := r.putJSON(ctx, refKey, ref, "", true); err != nil && !conflict(err) {
			return oci.Descriptor{}, err
		}
	}
	historyDesc := desc
	historyDesc.ArtifactType = info.artifactType
	for _, tag := range tags {
		if err := r.changeTag(ctx, repo, tag, historyDesc, oci.TagHistoryEventCreated, ""); err != nil {
			return oci.Descriptor{}, err
		}
	}
	return desc, nil
}

func (r *Registry) referrerKey(repo string, subject, digest oci.Digest) (string, error) {
	subjectKey, err := digestKey(subject)
	if err != nil {
		return "", err
	}
	key, err := digestKey(digest)
	if err != nil {
		return "", err
	}
	return r.repoKey(repo, "_referrers/"+strings.TrimSuffix(subjectKey, "/")+"/"+key), nil
}
