//go:build integration

package ocis3

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ohseeeye/oci/internal/conformance"
)

const minioImage = "chainguard/minio@sha256:9dcc028b309030afa86fc1fc8d93907ae373ea3fb75277cca3fc77e4645932b7"

func minioRegistry(t *testing.T) *Registry {
	t.Helper()
	if testing.Short() {
		t.Skip("S3 integration tests require Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "-p", "127.0.0.1::9000", "-e", "MINIO_ROOT_USER=oci-test", "-e", "MINIO_ROOT_PASSWORD=oci-test-password", minioImage, "server", "/data", "--address", ":9000").CombinedOutput()
	if err != nil {
		t.Fatalf("starting MinIO: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "stop", id).CombinedOutput()
		if err != nil {
			t.Errorf("stopping MinIO: %v\n%s", err, out)
		}
	})
	out, err = exec.CommandContext(ctx, "docker", "port", id, "9000/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("finding MinIO port: %v\n%s", err, out)
	}
	endpoint := "http://" + strings.TrimSpace(string(out))
	httpClient := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/minio/health/ready", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("MinIO did not become ready")
	}
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("oci-test", "oci-test-password", ""), RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	bucket := "oci-test-" + newID()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	r, err := New(client, bucket, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("S3 server: %s (%s)", endpoint, minioImage)
	return r
}

func TestOCIConformance(t *testing.T) { conformance.Run(t, "ocis3", minioRegistry(t)) }

func TestS3Integration(t *testing.T) {
	r := minioRegistry(t)
	exerciseUploads(t, r)
	desc := pushTaggedIndex(t, r, "history", "latest", "first")
	// Separate clients/registry instances must coordinate solely through S3.
	other := &Registry{client: r.client, bucket: r.bucket, prefix: r.prefix, partSize: r.partSize}
	if err := other.DeleteTag(t.Context(), "history", "latest"); err != nil {
		t.Fatal(err)
	}
	entries, err := collectHistory(t, r, "history", "latest")
	if err != nil || len(entries) != 2 || entries[0].Digest != desc.Digest {
		t.Fatalf("persistent history: %v %v", entries, err)
	}
	t.Run("concurrent tags", func(t *testing.T) { exerciseConcurrentTags(t, r) })
	t.Run("multi-part blob", func(t *testing.T) {
		data := strings.Repeat("x", r.partSize+17)
		blob := pushBlob(t, r, "large", data)
		br, err := r.GetBlobRange(t.Context(), "large", blob.Digest, int64(r.partSize-3), -1)
		got := readContent(t, br, err)
		if string(got) != strings.Repeat("x", 20) {
			t.Fatal(fmt.Sprintf("range across multipart boundary: %d bytes", len(got)))
		}
	})
}
