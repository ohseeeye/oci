package ocisqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/internal/ocitest"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func newRegistry(t *testing.T, dir string) *Registry {
	t.Helper()
	r, err := New(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

func pushBlob(t *testing.T, r oci.Registry, repo, data string) oci.Descriptor {
	t.Helper()
	desc := oci.Descriptor{Digest: ocidigest.FromBytes([]byte(data)), Size: int64(len(data)), MediaType: "application/octet-stream"}
	got, err := r.PushBlob(t.Context(), repo, desc, strings.NewReader(data))
	require.NoError(t, err)
	return got
}

func manifestBytes(t *testing.T, m oci.IndexOrManifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	require.NoError(t, err)
	return data
}

func readContent(t *testing.T, reader oci.BlobReader, err error) []byte {
	t.Helper()
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	return data
}

func TestSharedContentAndPersistence(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir, nil)
	require.NoError(t, err)
	config := pushBlob(t, r, "example/app", "{}")
	layer := pushBlob(t, r, "example/app", "layer")
	pushBlob(t, r, "other/app", "other")
	_, err = r.ResolveBlob(t.Context(), "other/app", layer.Digest)
	require.ErrorIs(t, err, oci.ErrBlobUnknown)
	config.MediaType = oci.MediaTypeImageConfig
	data := manifestBytes(t, oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &config, Layers: []oci.Descriptor{layer}})
	desc, err := r.PushManifest(t.Context(), "example/app", data, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{"v1", "latest"}})
	require.NoError(t, err)
	// Deleting one membership leaves mounted content available elsewhere.
	mounted, err := r.MountBlob(t.Context(), "example/app", "other/app", layer.Digest)
	require.NoError(t, err)
	require.Equal(t, layer, mounted)
	require.ErrorIs(t, r.DeleteBlob(t.Context(), "example/app", layer.Digest), oci.ErrDenied)
	require.NoError(t, r.DeleteBlob(t.Context(), "other/app", layer.Digest))
	require.NoError(t, r.Close())
	r = newRegistry(t, dir)
	got, err := r.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.Equal(t, desc, got)
	br, err := r.GetTag(t.Context(), "example/app", "latest")
	require.Equal(t, data, readContent(t, br, err))
	br, err = r.GetManifest(t.Context(), "example/app", desc.Digest)
	require.Equal(t, data, readContent(t, br, err))
	br, err = r.GetBlobRange(t.Context(), "example/app", layer.Digest, 1, 4)
	require.Equal(t, []byte("aye"), readContent(t, br, err))
	br, err = r.GetBlobRange(t.Context(), "example/app", layer.Digest, 3, 100)
	require.Equal(t, []byte("er"), readContent(t, br, err))
	_, err = r.GetBlobRange(t.Context(), "example/app", layer.Digest, -1, 2)
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	_, err = r.GetBlobRange(t.Context(), "example/app", layer.Digest, 6, -1)
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	tags, err := oci.All(r.Tags(t.Context(), "example/app", &oci.TagsParameters{StartAfter: "latest", Limit: 1}))
	require.NoError(t, err)
	require.Equal(t, []string{"v1"}, tags)
	repos, err := oci.All(r.Repositories(t.Context(), "example/app"))
	require.NoError(t, err)
	require.Equal(t, []string{"other/app"}, repos)
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	require.NoError(t, err)
	require.Len(t, entries, 4, "mounting must not copy content")
	require.FileExists(t, filepath.Join(dir, "metadata.db"))
	require.NoFileExists(t, filepath.Join(dir, "index.json"))
}

func TestConditionalManifestPush(t *testing.T) {
	ocitest.CheckConditionalManifestPush(t, newRegistry(t, t.TempDir()))
}

func TestConditionalManifestPushAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	first := newRegistry(t, dir)
	second := newRegistry(t, dir)
	old := pushTaggedIndex(t, first, "example", "latest", "initial")
	condition := strconv.Quote(old.Digest.String())
	start := make(chan struct{})
	type result struct {
		digest oci.Digest
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, r := range []*Registry{first, second} {
		wg.Go(func() {
			data := manifestBytes(t, oci.IndexOrManifest{
				SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex,
				Annotations: map[string]string{"version": fmt.Sprint(i)},
			})
			<-start
			_, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex,
				&oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: condition})
			results <- result{ocidigest.FromBytes(data), err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winner, loser oci.Digest
	for got := range results {
		if got.err == nil {
			require.Empty(t, winner, "only one writer may replace the observed tag")
			winner = got.digest
		} else {
			require.ErrorIs(t, got.err, oci.ErrManifestInvalid)
			require.ErrorContains(t, got.err, "If-Match does not match the current tag digest")
			loser = got.digest
		}
	}
	require.NotEmpty(t, winner)
	require.NotEmpty(t, loser)
	for _, r := range []*Registry{first, second} {
		current, err := r.ResolveTag(t.Context(), "example", "latest")
		require.NoError(t, err)
		require.Equal(t, winner, current.Digest)
		_, err = r.ResolveManifest(t.Context(), "example", loser)
		require.ErrorIs(t, err, oci.ErrManifestUnknown)
		_, err = r.ResolveBlob(t.Context(), "example", loser)
		require.ErrorIs(t, err, oci.ErrBlobUnknown)
		entries, err := oci.All(r.TagHistory(t.Context(), "example", "latest", nil))
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.Equal(t, winner, entries[0].Digest)
	}
	path, err := blobPath(dir, loser)
	require.NoError(t, err)
	require.NoFileExists(t, path, "failed conditions must not publish blob files")
}

func TestConditionalManifestPushMissingRepository(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)
	data := manifestBytes(t, oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex})
	digest := ocidigest.FromBytes(data)
	for _, tags := range [][]string{{"latest"}, {"latest", "latest"}} {
		_, err := r.PushManifest(t.Context(), "missing", data, oci.MediaTypeImageIndex,
			&oci.PushManifestParameters{Tags: tags, IfMatch: strconv.Quote(digest.String())})
		require.ErrorIs(t, err, oci.ErrManifestInvalid)
	}
	repos, err := oci.All(r.Repositories(t.Context(), ""))
	require.NoError(t, err)
	require.Empty(t, repos, "failed conditions must roll back repository creation")
	path, err := blobPath(dir, digest)
	require.NoError(t, err)
	require.NoFileExists(t, path)
}

func TestBlobValidationAndCancellation(t *testing.T) {
	r := newRegistry(t, t.TempDir())
	good := pushBlob(t, r, "example", "good")
	_, err := r.PushBlob(t.Context(), "example", good, strings.NewReader("evil"))
	require.ErrorIs(t, err, oci.ErrDigestInvalid, "duplicate content must still be verified")
	bad := good
	bad.Size++
	_, err = r.PushBlob(t.Context(), "example", bad, strings.NewReader("good"))
	require.ErrorIs(t, err, oci.ErrSizeInvalid)
	bad.Size = -1
	_, err = r.PushBlob(t.Context(), "example", bad, strings.NewReader("good"))
	require.ErrorIs(t, err, oci.ErrSizeInvalid)
	bad.Digest = "sha256:../../escape"
	bad.Size = 4
	_, err = r.PushBlob(t.Context(), "example", bad, strings.NewReader("good"))
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	_, err = r.PushBlob(t.Context(), "../bad", good, strings.NewReader("good"))
	require.ErrorIs(t, err, oci.ErrNameInvalid)
	empty := pushBlob(t, r, "example", "")
	br, err := r.GetBlob(t.Context(), "example", empty.Digest)
	require.Empty(t, readContent(t, br, err))
	dw, err := ocidigest.NewWriter(nil, ocidigest.SHA512)
	require.NoError(t, err)
	_, err = dw.Write([]byte("sha512"))
	require.NoError(t, err)
	digest, err := dw.Digest()
	require.NoError(t, err)
	_, err = r.PushBlob(t.Context(), "example", oci.Descriptor{Digest: digest, Size: 6}, strings.NewReader("sha512"))
	require.NoError(t, err)
	br, err = r.GetBlob(t.Context(), "example", digest)
	require.Equal(t, []byte("sha512"), readContent(t, br, err))
	ctx, cancel := context.WithCancel(t.Context())
	br, err = r.GetBlob(ctx, "example", good.Digest)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(br)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, br.Close())
	_, err = r.PushBlob(ctx, "example", good, strings.NewReader("good"))
	require.ErrorIs(t, err, context.Canceled)
	_, err = oci.All(r.Tags(ctx, "example", nil))
	require.ErrorIs(t, err, context.Canceled)
}

func TestManifestReferencesAndReferrers(t *testing.T) {
	r := newRegistry(t, t.TempDir())
	config := pushBlob(t, r, "source", "{}")
	config.MediaType = oci.MediaTypeImageConfig
	m := oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &config}
	data := manifestBytes(t, m)
	_, err := r.PushManifest(t.Context(), "target", data, m.MediaType, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	_, err = r.ResolveTag(t.Context(), "target", "latest")
	require.ErrorIs(t, err, oci.ErrNameUnknown, "failed writes must roll back repository creation")
	child, err := r.PushManifest(t.Context(), "source", data, m.MediaType, nil)
	require.NoError(t, err)
	index := oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: []oci.Descriptor{child}}
	idx, err := r.PushManifest(t.Context(), "source", manifestBytes(t, index), index.MediaType, nil)
	require.NoError(t, err)
	require.ErrorIs(t, r.DeleteManifest(t.Context(), "source", child.Digest), oci.ErrDenied)
	require.NoError(t, r.DeleteManifest(t.Context(), "source", idx.Digest))
	// A subject is allowed to be absent; annotations are repository scoped.
	subject := oci.Descriptor{Digest: ocidigest.FromBytes([]byte("absent")), Size: 6, MediaType: oci.MediaTypeImageManifest}
	m.Subject = &subject
	m.Annotations = map[string]string{"note": "test"}
	m.ArtifactType = "application/example"
	foreign := oci.Descriptor{Digest: ocidigest.FromBytes([]byte("foreign")), Size: 7, MediaType: "application/foreign", URLs: []string{"https://example.com/layer"}}
	m.Layers = []oci.Descriptor{foreign}
	ref, err := r.PushManifest(t.Context(), "source", manifestBytes(t, m), m.MediaType, nil)
	require.NoError(t, err)
	refs, err := oci.All(r.Referrers(t.Context(), "source", subject.Digest, nil))
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, ref.Digest, refs[0].Digest)
	require.Equal(t, m.Annotations, refs[0].Annotations)
	require.Equal(t, m.ArtifactType, refs[0].ArtifactType)
	refs, err = oci.All(r.Referrers(t.Context(), "source", subject.Digest, &oci.ReferrersParameters{ArtifactType: "application/other"}))
	require.NoError(t, err)
	require.Empty(t, refs)
	_, err = r.MountBlob(t.Context(), "source", "target", config.Digest)
	require.NoError(t, err)
	_, err = r.PushManifest(t.Context(), "target", manifestBytes(t, m), m.MediaType, nil)
	require.NoError(t, err, "same manifest annotations must work in multiple repositories")
	require.NoError(t, r.DeleteManifest(t.Context(), "source", ref.Digest))
	refs, err = oci.All(r.Referrers(t.Context(), "target", subject.Digest, nil))
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.NoError(t, r.DeleteManifest(t.Context(), "source", child.Digest))
	require.NoError(t, r.DeleteBlob(t.Context(), "source", config.Digest))
	br, err := r.GetBlob(t.Context(), "target", config.Digest)
	require.Equal(t, []byte("{}"), readContent(t, br, err))
}

func TestConcurrentRegistryInstances(t *testing.T) {
	dir := t.TempDir()
	first := newRegistry(t, dir)
	second := newRegistry(t, dir)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			r := first
			if i%2 == 1 {
				r = second
			}
			data := []byte("shared content")
			desc := oci.Descriptor{Digest: ocidigest.FromBytes(data), Size: int64(len(data))}
			_, err := r.PushBlob(t.Context(), fmt.Sprintf("repo%d", i), desc, bytes.NewReader(data))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	repos, err := oci.All(first.Repositories(t.Context(), ""))
	require.NoError(t, err)
	require.Len(t, repos, 16)
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, first.withConn(t.Context(), false, func(conn *sqlite.Conn) error {
		n, err := integer(conn, "PRAGMA foreign_keys")
		require.EqualValues(t, 1, n)
		return err
	}))
}

func TestOpenValidation(t *testing.T) {
	_, err := New("", nil)
	require.Error(t, err)
	_, err = New(t.TempDir(), &Options{PoolSize: -1})
	require.Error(t, err)
	dir := t.TempDir()
	r, err := New(dir, &Options{PoolSize: 1})
	require.NoError(t, err)
	require.NoError(t, r.withConn(t.Context(), true, func(conn *sqlite.Conn) error { return execute(conn, "PRAGMA user_version=3") }))
	require.NoError(t, r.Close())
	_, err = New(dir, nil)
	require.ErrorContains(t, err, "unsupported metadata schema version")
}

func TestSparseManifests(t *testing.T) {
	r, err := New(t.TempDir(), &Options{AllowSparseManifests: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	ocitest.CheckSparseManifests(t, r)
}

func TestMigrateSparseSchema(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir, nil)
	require.NoError(t, err)
	blob := pushBlob(t, r, "app", "config")
	data := manifestBytes(t, oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &blob, Layers: []oci.Descriptor{}})
	child, err := r.PushManifest(t.Context(), "app", data, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{"child"}})
	require.NoError(t, err)
	index := manifestBytes(t, oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: []oci.Descriptor{child}, Annotations: map[string]string{"example": "preserved"}})
	parent, err := r.PushManifest(t.Context(), "app", index, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	// Recreate the original child constraints with existing relationships, then
	// reopen through New to exercise a real version-one database upgrade.
	require.NoError(t, r.withConn(t.Context(), true, func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteScript(conn, `
CREATE TABLE old_manifest_blob (
 repository_id INTEGER NOT NULL, manifest TEXT NOT NULL, blob_digest TEXT NOT NULL,
 PRIMARY KEY(repository_id,manifest,blob_digest),
 FOREIGN KEY(repository_id,manifest) REFERENCES manifests(repository_id,digest) ON DELETE CASCADE,
 FOREIGN KEY(repository_id,blob_digest) REFERENCES repository_blob(repository_id,digest)
);
INSERT INTO old_manifest_blob SELECT * FROM manifest_blob;
DROP TABLE manifest_blob;
ALTER TABLE old_manifest_blob RENAME TO manifest_blob;
CREATE INDEX manifest_blob_digest ON manifest_blob(repository_id,blob_digest);
CREATE TABLE old_manifest_manifest (
 repository_id INTEGER NOT NULL, manifest TEXT NOT NULL, child_digest TEXT NOT NULL,
 PRIMARY KEY(repository_id,manifest,child_digest),
 FOREIGN KEY(repository_id,manifest) REFERENCES manifests(repository_id,digest) ON DELETE CASCADE,
 FOREIGN KEY(repository_id,child_digest) REFERENCES manifests(repository_id,digest)
);
INSERT INTO old_manifest_manifest SELECT * FROM manifest_manifest;
DROP TABLE manifest_manifest;
ALTER TABLE old_manifest_manifest RENAME TO manifest_manifest;
CREATE INDEX manifest_manifest_child ON manifest_manifest(repository_id,child_digest);
PRAGMA user_version=1;
`, nil)
	}))
	require.NoError(t, r.Close())
	r, err = New(dir, nil)
	require.NoError(t, err)
	got, err := r.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	require.Equal(t, parent.Digest, got.Digest)
	reader, err := r.GetManifest(t.Context(), "app", parent.Digest)
	require.Equal(t, index, readContent(t, reader, err))
	history, err := oci.All(r.TagHistory(t.Context(), "app", "latest", nil))
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.ErrorIs(t, r.DeleteBlob(t.Context(), "app", blob.Digest), oci.ErrDenied)
	require.ErrorIs(t, r.DeleteManifest(t.Context(), "app", child.Digest), oci.ErrDenied)
	require.NoError(t, r.withConn(t.Context(), false, func(conn *sqlite.Conn) error {
		version, err := integer(conn, "PRAGMA user_version")
		require.NoError(t, err)
		require.EqualValues(t, 2, version)
		blobs, err := integer(conn, "SELECT count(*) FROM manifest_blob")
		require.NoError(t, err)
		require.EqualValues(t, 1, blobs)
		manifests, err := integer(conn, "SELECT count(*) FROM manifest_manifest")
		require.NoError(t, err)
		require.EqualValues(t, 1, manifests)
		return rows(conn, "PRAGMA foreign_key_check", func(*sqlite.Stmt) error {
			t.Error("foreign key violation after migration")
			return nil
		})
	}))
	require.NoError(t, r.Close())
	r, err = New(dir, &Options{AllowSparseManifests: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.NoError(t, r.DeleteManifest(t.Context(), "app", child.Digest))
	require.NoError(t, r.DeleteBlob(t.Context(), "app", blob.Digest))
	reader, err = r.GetManifest(t.Context(), "app", parent.Digest)
	require.Equal(t, index, readContent(t, reader, err))
	// Strict mode still checks relationships after the schema migration.
	strict := newRegistry(t, dir)
	_, err = strict.PushManifest(t.Context(), "app", index, oci.MediaTypeImageIndex, nil)
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
}
