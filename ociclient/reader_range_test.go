package ociclient

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ocitransfer"
	"github.com/stretchr/testify/require"
)

func TestGetBlobRange(t *testing.T) {
	const data = "0123456789abcdef"
	digest := ocidigest.FromBytes([]byte(data))
	for _, supportsRange := range []bool{false, true} {
		name := "ignores Range"
		if supportsRange {
			name = "supports Range"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Docker-Content-Digest", digest.String())
				if supportsRange {
					http.ServeContent(w, r, "blob", time.Time{}, strings.NewReader(data))
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = io.WriteString(w, data)
			}))
			t.Cleanup(server.Close)
			client, err := New(strings.TrimPrefix(server.URL, "http://"), &Options{Insecure: true})
			require.NoError(t, err)
			for _, test := range []struct {
				name       string
				start, end int64
				want       string
			}{
				{"prefix", 0, 4, "0123"},
				{"middle", 3, 8, "34567"},
				{"last byte", 15, 16, "f"},
				{"open end", 8, -1, "89abcdef"},
				{"negative end", 8, math.MinInt64, "89abcdef"},
				{"end beyond size", 8, 100, "89abcdef"},
				{"maximum end", 8, math.MaxInt64, "89abcdef"},
				{"full bounded", 0, 16, data},
				{"full unbounded", 0, -1, data},
				{"empty start", 0, 0, ""},
				{"empty middle", 8, 8, ""},
				{"empty at end", 16, 16, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					reader, err := client.GetBlobRange(t.Context(), "example", digest, test.start, test.end)
					require.NoError(t, err)
					t.Cleanup(func() { _ = reader.Close() })
					require.Equal(t, oci.Descriptor{Digest: digest, Size: int64(len(data)), MediaType: "application/octet-stream"}, reader.Descriptor())
					got, err := io.ReadAll(reader)
					require.NoError(t, err)
					require.Equal(t, test.want, string(got))
					require.NoError(t, reader.Close())
				})
			}
		})
	}
}

type rangeTestBody struct {
	io.Reader
	read     int
	closed   bool
	closeErr error
}

func (r *rangeTestBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func (r *rangeTestBody) Close() error {
	r.closed = true
	return r.closeErr
}

func TestGetBlobRangeFallbackStreamsAndCloses(t *testing.T) {
	const data = "0123456789abcdef"
	for _, test := range []struct {
		name                  string
		start, end            int64
		wantHeader, want      string
		beforeRead, afterRead int
		skipRead              bool
	}{
		{name: "bounded", start: 4, end: 8, wantHeader: "bytes=4-7", want: "4567", beforeRead: 4, afterRead: 8},
		{name: "early close", start: 4, end: 8, wantHeader: "bytes=4-7", beforeRead: 4, afterRead: 4, skipRead: true},
		{name: "empty", start: 4, end: 4, beforeRead: 0, afterRead: 0},
		{name: "open", start: 4, end: -1, wantHeader: "bytes=4-", want: "456789abcdef", beforeRead: 4, afterRead: len(data)},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &rangeTestBody{Reader: strings.NewReader(data), closeErr: errors.New("close failed")}
			var requests int
			client, err := New("registry.example", &Options{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, test.wantHeader, req.Header.Get("Range"))
				return &http.Response{
					StatusCode: http.StatusOK, ContentLength: int64(len(data)),
					Header: make(http.Header), Body: body, Request: req,
				}, nil
			})})
			require.NoError(t, err)
			reader, err := client.GetBlobRange(t.Context(), "example", ocidigest.FromBytes([]byte(data)), test.start, test.end)
			require.NoError(t, err)
			require.Equal(t, test.beforeRead, body.read)
			require.False(t, body.closed)
			if !test.skipRead {
				got, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.Equal(t, test.want, string(got))
			}
			require.Equal(t, test.afterRead, body.read)
			require.ErrorIs(t, reader.Close(), body.closeErr)
			require.True(t, body.closed)
			require.Equal(t, 1, requests, "reuse the full response rather than issuing another GET")
		})
	}
}

func TestGetBlobRangeRejectsInvalidRanges(t *testing.T) {
	client, err := New("registry.example", &Options{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid range reached transport")
		return nil, nil
	})})
	require.NoError(t, err)
	for _, bounds := range [][2]int64{{-1, -1}, {-1, 5}, {5, 4}, {5, 0}} {
		reader, err := client.GetBlobRange(t.Context(), "example", ocidigest.FromBytes([]byte("blob")), bounds[0], bounds[1])
		require.Nil(t, reader)
		require.ErrorIs(t, err, oci.ErrRangeInvalid)
	}
}

func TestGetBlobRangeFallbackErrors(t *testing.T) {
	readErr := errors.New("body read failed")
	for _, test := range []struct {
		name       string
		body       io.Reader
		size       int64
		start, end int64
		wantErr    error
		read       bool
	}{
		{name: "unknown size", body: strings.NewReader("abc"), size: -1, start: 1, end: 2},
		{name: "past end", body: strings.NewReader("abc"), size: 3, start: 4, end: -1, wantErr: oci.ErrRangeInvalid},
		{name: "empty past end", body: strings.NewReader("abc"), size: 3, start: 4, end: 4, wantErr: oci.ErrRangeInvalid},
		{name: "short prefix", body: strings.NewReader("ab"), size: 10, start: 4, end: 8, wantErr: io.ErrUnexpectedEOF},
		{name: "prefix error", body: iotest.ErrReader(readErr), size: 10, start: 4, end: 8, wantErr: readErr},
		{name: "short range", body: strings.NewReader("abcde"), size: 10, start: 2, end: 8, wantErr: io.ErrUnexpectedEOF, read: true},
		{name: "range error", body: io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(readErr)), size: 10, start: 2, end: 8, wantErr: readErr, read: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &rangeTestBody{Reader: test.body}
			client, err := New("registry.example", &Options{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, ContentLength: test.size,
					Header: make(http.Header), Body: body, Request: req,
				}, nil
			})})
			require.NoError(t, err)
			reader, err := client.GetBlobRange(t.Context(), "example", ocidigest.FromBytes([]byte("blob")), test.start, test.end)
			if test.read {
				require.NoError(t, err)
				_, err = io.ReadAll(reader)
				require.NoError(t, reader.Close())
			} else {
				require.Nil(t, reader)
			}
			require.Error(t, err)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			}
			require.True(t, body.closed, "failed calls must close the response; returned readers retain Close")
		})
	}
}

func TestGetBlobRangeFallbackCancellation(t *testing.T) {
	for _, start := range []int64{0, 4} {
		t.Run(strconv.FormatInt(start, 10), func(t *testing.T) {
			ready := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "16")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(ready)
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			client, err := New(strings.TrimPrefix(server.URL, "http://"), &Options{Insecure: true})
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				reader, err := client.GetBlobRange(ctx, "example", ocidigest.FromBytes([]byte("blob")), start, 8)
				if err == nil {
					_, err = io.ReadAll(reader)
					_ = reader.Close()
				}
				done <- err
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach server")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("canceling did not stop the range read")
			}
		})
	}
}

func TestDownloadBlobWhenRegistryIgnoresRange(t *testing.T) {
	data := strings.Repeat("0123456789abcdef", 32)
	digest := ocidigest.FromBytes([]byte(data))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", digest.String())
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, data)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(strings.TrimPrefix(server.URL, "http://"), &Options{Insecure: true})
	require.NoError(t, err)
	reader, err := ocitransfer.DownloadBlob(t.Context(), client, "example", digest, &ocitransfer.DownloadOptions{Concurrency: 3, ChunkSize: 32, MaxAttempts: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, data, string(got))
	require.Equal(t, int64(len(data)), reader.Descriptor().Size)
	require.Equal(t, digest, reader.Descriptor().Digest)
	require.NoError(t, reader.Close())
}
