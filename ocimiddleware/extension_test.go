package ocimiddleware_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ocimiddleware"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var historyWrappers = []struct {
	name string
	repo string
	wrap func(oci.Registry) oci.Registry
}{
	{"AccessChecker", "tenant/app", func(r oci.Registry) oci.Registry {
		return ocimiddleware.AccessChecker(r, func(string, ocimiddleware.AccessKind) error { return nil })
	}},
	{"Select", "tenant/app", func(r oci.Registry) oci.Registry {
		return ocimiddleware.Select(r, func(repo string) bool { return repo == "tenant/app" })
	}},
	{"Sub", "app", func(r oci.Registry) oci.Registry { return ocimiddleware.Sub(r, "tenant") }},
	{"Debug", "tenant/app", func(r oci.Registry) oci.Registry {
		return ocimiddleware.Debug(r, func(string, ...any) {})
	}},
	{"ReadOnly", "tenant/app", ocimiddleware.ReadOnly},
	{"Immutable", "tenant/app", ocimiddleware.Immutable},
	{"Selector", "tenant/app", func(r oci.Registry) oci.Registry {
		return ocimiddleware.Selector(func(string) oci.Registry { return r })
	}},
}

func requireTagHistory(t *testing.T, r oci.Registry) oci.TagHistory {
	t.Helper()
	history, ok := r.(oci.TagHistory)
	require.True(t, ok, "registry wrapper must expose TagHistory")
	return history
}

func seedTagHistory(t *testing.T, repo string) (*ocimem.Registry, []oci.Descriptor) {
	t.Helper()
	r := ocimem.New()
	for _, version := range []string{"same", "same", "other", "same", "same"} {
		data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, version))
		_, err := r.PushManifest(t.Context(), repo, data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"release"}})
		require.NoError(t, err)
	}
	require.NoError(t, r.DeleteTag(t.Context(), repo, "release"))
	entries, err := oci.All(r.TagHistory(t.Context(), repo, "release", nil))
	require.NoError(t, err)
	require.Len(t, entries, 6)
	return r, entries
}

func historyTimestamp(t *testing.T, desc oci.Descriptor) time.Time {
	t.Helper()
	timestamp, err := time.Parse(time.RFC3339Nano, desc.Annotations[oci.TagHistoryTimestampAnnotation])
	require.NoError(t, err)
	return timestamp
}

func TestTagHistoryWrappersForwardFiltersAndLimits(t *testing.T) {
	backend, entries := seedTagHistory(t, "tenant/app")
	limit, zero := 2, 0
	for _, wrapper := range historyWrappers {
		t.Run(wrapper.name, func(t *testing.T) {
			history := requireTagHistory(t, wrapper.wrap(backend))
			for _, query := range []struct {
				name   string
				params *oci.TagHistoryParameters
				want   []oci.Descriptor
			}{
				{"all", nil, entries},
				{"filters", &oci.TagHistoryParameters{
					Before: historyTimestamp(t, entries[0]),
					Since:  historyTimestamp(t, entries[5]),
					Digest: entries[1].Digest,
				}, []oci.Descriptor{entries[1], entries[2], entries[4]}},
				{"limit", &oci.TagHistoryParameters{Limit: &limit}, entries[:2]},
				{"zero limit", &oci.TagHistoryParameters{Limit: &zero}, nil},
			} {
				t.Run(query.name, func(t *testing.T) {
					got, err := oci.All(history.TagHistory(t.Context(), wrapper.repo, "release", query.params))
					require.NoError(t, err)
					if len(query.want) == 0 {
						require.Empty(t, got)
					} else {
						require.Equal(t, query.want, got)
					}
				})
			}
		})
	}
}

func TestReadOnlyPreservesCoreReadAndWritePolicy(t *testing.T) {
	backend, entries := seedTagHistory(t, "tenant/app")
	r := ocimiddleware.ReadOnly(backend)
	reader, err := r.GetManifest(t.Context(), "tenant/app", entries[0].Digest)
	require.NoError(t, err)
	require.Equal(t, entries[0].Digest, reader.Descriptor().Digest)
	require.NoError(t, reader.Close())
	_, err = oci.All(r.Tags(t.Context(), "tenant/app", nil))
	require.NoError(t, err)

	require.ErrorIs(t, r.DeleteManifest(t.Context(), "tenant/app", entries[0].Digest), oci.ErrUnsupported)
	data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[]}`, oci.MediaTypeImageIndex))
	_, err = r.PushManifest(t.Context(), "tenant/app", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"new"}})
	require.ErrorIs(t, err, oci.ErrUnsupported)
	_, err = backend.ResolveManifest(t.Context(), "tenant/app", entries[0].Digest)
	require.NoError(t, err)
	_, err = backend.ResolveTag(t.Context(), "tenant/app", "new")
	require.ErrorIs(t, err, oci.ErrManifestUnknown)
}

func TestTagHistoryWrappersWithoutCapability(t *testing.T) {
	for _, wrapper := range historyWrappers {
		t.Run(wrapper.name, func(t *testing.T) {
			history := requireTagHistory(t, wrapper.wrap(&oci.Funcs{}))
			zero := 0
			for _, params := range []*oci.TagHistoryParameters{nil, {Limit: &zero}} {
				_, err := oci.All(history.TagHistory(t.Context(), wrapper.repo, "release", params))
				require.ErrorIs(t, err, oci.ErrUnsupported)
			}
		})
	}
}

func TestTagHistoryWrappersPropagateUpstreamCancellation(t *testing.T) {
	backend := ocimem.New()
	for _, wrapper := range historyWrappers {
		t.Run(wrapper.name, func(t *testing.T) {
			history := requireTagHistory(t, wrapper.wrap(backend))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := oci.All(history.TagHistory(ctx, wrapper.repo, "release", nil))
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

type historyObserver struct {
	*ocimem.Registry
	observe func(context.Context, string)
}

func (r historyObserver) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	r.observe(ctx, repo)
	return r.Registry.TagHistory(ctx, repo, tag, params)
}

func TestTagHistoryAccessDenialDoesNotReachBackend(t *testing.T) {
	denied := errors.New("history access denied")
	for _, tc := range []struct {
		name string
		wrap func(oci.Registry) oci.Registry
		want error
	}{
		{"AccessChecker", func(r oci.Registry) oci.Registry {
			return ocimiddleware.AccessChecker(r, func(repo string, access ocimiddleware.AccessKind) error {
				if repo == "private/app" && access == ocimiddleware.AccessList {
					return denied
				}
				return nil
			})
		}, denied},
		{"Select", func(r oci.Registry) oci.Registry {
			return ocimiddleware.Select(r, func(repo string) bool { return repo == "public/app" })
		}, oci.ErrNameUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reachedBackend bool
			r := tc.wrap(historyObserver{ocimem.New(), func(context.Context, string) { reachedBackend = true }})
			_, err := oci.All(requireTagHistory(t, r).TagHistory(t.Context(), "private/app", "release", nil))
			require.ErrorIs(t, err, tc.want)
			require.False(t, reachedBackend, "denied history must not reach the backend")
		})
	}
}

func TestAccessCheckerTagHistoryDenialPrecedesUnsupported(t *testing.T) {
	r := ocimiddleware.AccessChecker(&oci.Funcs{}, func(string, ocimiddleware.AccessKind) error { return oci.ErrDenied })
	_, err := oci.All(requireTagHistory(t, r).TagHistory(t.Context(), "private/app", "release", nil))
	require.ErrorIs(t, err, oci.ErrDenied)
}

func TestSubTagHistoryMapsRepositoryAndScopes(t *testing.T) {
	backend, want := seedTagHistory(t, "tenant/project/app")
	var gotRepo string
	var gotScope ociauth.Scope
	r := ocimiddleware.Sub(historyObserver{backend, func(ctx context.Context, repo string) {
		gotRepo, gotScope = repo, ociauth.ScopeFromContext(ctx)
	}}, "tenant/project")
	const scope = "other registry:catalog:* repository:app:pull repository:elsewhere:push,delete"
	ctx := ociauth.ContextWithScope(t.Context(), ociauth.ParseScope(scope))
	got, err := oci.All(requireTagHistory(t, r).TagHistory(ctx, "app", "release", nil))
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, "tenant/project/app", gotRepo)
	wantScope := ociauth.ParseScope("other registry:catalog:* repository:tenant/project/app:pull repository:tenant/project/elsewhere:push,delete")
	require.True(t, wantScope.Equal(gotScope), "got scope %v, want %v", gotScope, wantScope)
	require.True(t, ociauth.ParseScope(scope).Equal(ociauth.ScopeFromContext(ctx)), "caller scope must remain unchanged")
}

func TestSelectorTagHistoryRoutesByRepository(t *testing.T) {
	local, localEntries := seedTagHistory(t, "local/app")
	remote, remoteEntries := seedTagHistory(t, "remote/app")
	r := ocimiddleware.Selector(func(repo string) oci.Registry {
		if strings.HasPrefix(repo, "local/") {
			return local
		}
		if strings.HasPrefix(repo, "remote/") {
			return remote
		}
		return &oci.Funcs{}
	})
	history := requireTagHistory(t, r)
	for _, tc := range []struct {
		repo string
		want []oci.Descriptor
	}{{"local/app", localEntries}, {"remote/app", remoteEntries}} {
		got, err := oci.All(history.TagHistory(t.Context(), tc.repo, "release", nil))
		require.NoError(t, err)
		require.Equal(t, tc.want, got, tc.repo)
	}
	_, err := oci.All(history.TagHistory(t.Context(), "legacy/app", "release", nil))
	require.ErrorIs(t, err, oci.ErrUnsupported)
}

func TestDebugTagHistoryLogsOperationResultsAndErrors(t *testing.T) {
	backend, want := seedTagHistory(t, "tenant/app")
	var logs strings.Builder
	r := ocimiddleware.Debug(backend, func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	})
	history := requireTagHistory(t, r)
	got, err := oci.All(history.TagHistory(t.Context(), "tenant/app", "release", nil))
	require.NoError(t, err)
	require.Equal(t, want, got)
	for _, text := range []string{"TagHistory", "tenant/app", "release", want[0].Digest.String(), oci.TagHistoryEventDeleted} {
		assert.Contains(t, logs.String(), text)
	}
	logs.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = oci.All(history.TagHistory(ctx, "tenant/app", "release", nil))
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, logs.String(), "TagHistory")
	assert.Contains(t, logs.String(), context.Canceled.Error())
}

func TestTagHistoryRoundTripThroughWrappedBackend(t *testing.T) {
	backend, want := seedTagHistory(t, "tenant/app")
	wrapped := ocimiddleware.Sub(ocimiddleware.ReadOnly(ocimiddleware.AccessChecker(backend, func(repo string, access ocimiddleware.AccessKind) error {
		if repo == "tenant/app" && access == ocimiddleware.AccessList {
			return nil
		}
		return oci.ErrDenied
	})), "tenant")
	server, err := ociserver.New(wrapped, nil)
	require.NoError(t, err)
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client, err := ociclient.New(u.Host, &ociclient.Options{Insecure: true})
	require.NoError(t, err)
	got, err := oci.All(client.TagHistory(t.Context(), "app", "release", nil))
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, err = oci.All(client.TagHistory(t.Context(), "private", "release", nil))
	require.ErrorIs(t, err, oci.ErrDenied)
}
