package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mirrorOpts configures the synthetic archive served by newTestMirror.
type mirrorOpts struct {
	Codename     string        // defaults to "noble"
	Arch         string        // defaults to "amd64"
	Components   []string      // defaults to main, restricted, universe, multiverse
	ReleaseDate  time.Time     // Date: field in InRelease; defaults to now
	Marker       string        // marker filename suffix; empty means no marker
	MarkerAge    time.Duration // how long ago the marker was modified
	MarkerFlaps  bool          // serve the marker on every other root request
	CorruptIndex string        // index path served with bytes that do not match InRelease
	MissingIndex string        // index InRelease declares but the mirror 404s: a half-finished tree
	ContentsSize int64         // size of Contents-<arch>.gz; zero omits the file
	OnlyRelease  bool          // publish the detached Release file only, 404 InRelease
}

// testMirror is the in-memory archive backing one httptest.Server.
type testMirror struct {
	opts     mirrorOpts
	indexes  map[string][]byte // path relative to dists/<codename>/ -> served bytes
	hashes   map[string]string // same path -> SHA256 InRelease declares
	sizes    map[string]int64
	rootHits atomic.Int64
}

// newTestMirror starts an httptest.Server serving a small but structurally
// faithful Ubuntu archive, and returns its base URL with a trailing slash.
func newTestMirror(t *testing.T, opts mirrorOpts) string {
	t.Helper()
	if opts.Codename == "" {
		opts.Codename = "noble"
	}
	if opts.Arch == "" {
		opts.Arch = "amd64"
	}
	if opts.Components == nil {
		opts.Components = []string{"main", "restricted", "universe", "multiverse"}
	}
	if opts.ReleaseDate.IsZero() {
		opts.ReleaseDate = time.Now().UTC().Truncate(time.Second)
	}

	m := &testMirror{
		opts:    opts,
		indexes: map[string][]byte{},
		hashes:  map[string]string{},
		sizes:   map[string]int64{},
	}
	for _, c := range opts.Components {
		m.addIndex(t, c+"/binary-"+opts.Arch+"/Packages.gz", "Package: example-"+c+"\n")
		m.addIndex(t, c+"/i18n/Translation-en.gz", "Package: example-"+c+"\nDescription-en: x\n")
	}

	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// addIndex gzips body, records the hash InRelease will declare, and stores the
// bytes actually served — which differ when the index is marked corrupt.
func (m *testMirror) addIndex(t *testing.T, path, body string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	good := buf.Bytes()
	sum := sha256.Sum256(good)
	m.hashes[path] = hex.EncodeToString(sum[:])
	m.sizes[path] = int64(len(good))
	served := good
	if path == m.opts.CorruptIndex {
		served = append(append([]byte{}, good...), 'X')
	}
	m.indexes[path] = served
}

func (m *testMirror) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	prefix := "dists/" + m.opts.Codename + "/"

	switch {
	case path == "":
		hits := m.rootHits.Add(1)
		show := m.opts.Marker != "" && (!m.opts.MarkerFlaps || hits%2 == 1)
		_, _ = fmt.Fprint(w, `<html><body><pre><a href="dists/">dists/</a>`)
		if show {
			name := "Archive-Update-in-Progress-" + m.opts.Marker
			_, _ = fmt.Fprintf(w, `<a href="%s">%s</a>`, name, name)
		}
		_, _ = fmt.Fprint(w, `</pre></body></html>`)

	case strings.HasPrefix(path, "Archive-Update-in-Progress-"):
		if m.opts.Marker == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, path, time.Now().Add(-m.opts.MarkerAge), strings.NewReader("x"))

	case path == prefix+"InRelease":
		if m.opts.OnlyRelease {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "InRelease", time.Now(), strings.NewReader(m.inRelease()))

	case path == prefix+"Release":
		if !m.opts.OnlyRelease {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "Release", time.Now(), strings.NewReader(m.inRelease()))

	case path == prefix+"Contents-"+m.opts.Arch+".gz":
		if m.opts.ContentsSize == 0 {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "Contents.gz", time.Now(),
			bytes.NewReader(bytes.Repeat([]byte("a"), int(m.opts.ContentsSize))))

	case strings.HasPrefix(path, prefix):
		rel := strings.TrimPrefix(path, prefix)
		body, ok := m.indexes[rel]
		if !ok || rel == m.opts.MissingIndex {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, rel, time.Now(), bytes.NewReader(body))

	default:
		http.NotFound(w, r)
	}
}

// inRelease renders a clearsigned-looking InRelease with a SHA256 stanza.
func (m *testMirror) inRelease() string {
	var b strings.Builder
	b.WriteString("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\n")
	fmt.Fprintf(&b, "Origin: Ubuntu\nSuite: %s\n", m.opts.Codename)
	fmt.Fprintf(&b, "Date: %s\n", m.opts.ReleaseDate.Format("Mon, 02 Jan 2006 15:04:05 MST"))
	b.WriteString("SHA256:\n")
	paths := make([]string, 0, len(m.hashes))
	for path := range m.hashes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		fmt.Fprintf(&b, " %s %d %s\n", m.hashes[path], m.sizes[path], path)
	}
	b.WriteString("-----BEGIN PGP SIGNATURE-----\nfake\n-----END PGP SIGNATURE-----\n")
	return b.String()
}

func TestTestMirrorServesInRelease(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble"})
	resp, err := http.Get(base + "dists/noble/InRelease") //nolint:noctx // test helper.
	if err != nil {
		t.Fatalf("get InRelease: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("InRelease: HTTP %d", resp.StatusCode)
	}
}

func TestTestMirrorConcurrentRootRequests(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Marker: "test-marker", MarkerFlaps: true})
	const numRequests = 20
	done := make(chan bool, numRequests)
	for range numRequests {
		go func() {
			resp, err := http.Get(base) //nolint:noctx,gosec // test helper with known safe URL.
			if err != nil {
				t.Errorf("concurrent get root: %v", err)
			} else {
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("root: HTTP %d", resp.StatusCode)
				}
			}
			done <- true
		}()
	}
	for range numRequests {
		<-done
	}
}
