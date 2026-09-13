package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectMarkerAbsent(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{})
	got, err := detectMarker(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerNone {
		t.Errorf("State = %v, want markerNone", got.State)
	}
}

func TestDetectMarkerFresh(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Marker: "host-1", MarkerAge: 5 * time.Minute})
	got, err := detectMarker(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerFresh {
		t.Errorf("State = %v, want markerFresh", got.State)
	}
	if got.Name != "Archive-Update-in-Progress-host-1" {
		t.Errorf("Name = %q", got.Name)
	}
}

func TestDetectMarkerStale(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Marker: "host-1", MarkerAge: 3 * time.Hour})
	got, err := detectMarker(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerStale {
		t.Errorf("State = %v, want markerStale", got.State)
	}
}

// A pooled host shows the marker on only some backends. One hit must not be
// enough, or archive.ubuntu.com flaps between runs.
func TestDetectMarkerIgnoresFlapping(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Marker: "host-1", MarkerAge: time.Minute, MarkerFlaps: true})
	got, err := detectMarker(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerNone {
		t.Errorf("State = %v, want markerNone for a flapping pool", got.State)
	}
}

// A confirmation that fails to answer is not an answer that the marker is
// gone. Losing the sighting would report a mirror that is genuinely mid-rsync
// as one whose root could not be checked, which skips index verification and
// ranks it as though nothing had been seen.
func TestDetectMarkerKeepsSightingWhenConfirmationFails(t *testing.T) {
	const name = "Archive-Update-in-Progress-host-1"
	var hits atomic.Int64
	var headHits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headHits.Add(1)
		}
		if hits.Add(1) > 1 {
			// Drop the connection without a response: the confirming request,
			// and the HEAD that would read the marker's age, both fail.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = fmt.Fprintf(w, `<html><body><pre><a href="dists/">dists/</a>`+
			`<a href="%s">%s</a></pre></body></html>`, name, name)
	}))
	t.Cleanup(srv.Close)

	got, err := detectMarker(t.Context(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerStale {
		t.Errorf("State = %v, want markerStale: a marker was seen and its age could not be read", got.State)
	}
	if got.Name != name {
		t.Errorf("Name = %q, want %q: the sighting must name the file it saw", got.Name, name)
	}
	if headHits.Load() == 0 {
		t.Error("marker age was not requested after confirmation failed")
	}
}

// A failed confirmation must not force a recent marker into stale-lock when
// the original connection can still provide its age. The first client has the
// only connection that saw the marker, so the fallback HEAD must use it.
func TestDetectMarkerReadsAgeFromSightingWhenConfirmationFails(t *testing.T) {
	type connKey struct{}
	const name = "Archive-Update-in-Progress-host-1"
	var conns atomic.Int64
	var headHits atomic.Int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(connKey{}).(int64)
		switch {
		case id == 1 && r.Method == http.MethodGet && r.URL.Path == "/":
			_, _ = fmt.Fprintf(w, `<html><body><pre><a href="%s">%s</a></pre></body></html>`, name, name)
		case id == 1 && r.Method == http.MethodHead && r.URL.Path == "/"+name:
			headHits.Add(1)
			w.Header().Set("Last-Modified", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
		default:
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, conns.Add(1))
	}
	srv.Start()
	t.Cleanup(srv.Close)

	got, err := detectMarker(t.Context(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerFresh {
		t.Errorf("State = %v, want markerFresh: the original connection supplied a recent age", got.State)
	}
	if got.Name != name {
		t.Errorf("Name = %q, want %q", got.Name, name)
	}
	if headHits.Load() != 1 {
		t.Errorf("marker HEAD requests on the original connection = %d, want 1", headHits.Load())
	}
}

func TestParseInReleaseIndexes(t *testing.T) {
	data := []byte("Origin: Ubuntu\nSHA256:\n" +
		" aaaa 12 main/binary-amd64/Packages.gz\n" +
		" bbbb 34 main/i18n/Translation-en.gz\n" +
		"Acquire-By-Hash: yes\n")
	refs := parseInReleaseIndexes(data)
	if len(refs) != 2 {
		t.Fatalf("len = %d, want 2", len(refs))
	}
	if refs[0].SHA256 != "aaaa" || refs[0].Size != 12 || refs[0].Path != "main/binary-amd64/Packages.gz" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
}

// Only the indexes apt reads on this architecture are verified: verifying
// everything InRelease lists is 444 files and 8.4 GB on a real archive.
func TestVerifiableIndexesSelectsArchAndI18n(t *testing.T) {
	refs := []indexRef{
		{Path: "main/binary-amd64/Packages.gz"},
		{Path: "main/binary-arm64/Packages.gz"},
		{Path: "main/i18n/Translation-en.gz"},
		{Path: "main/source/Sources.gz"},
		{Path: "Contents-amd64"},
	}
	got := verifiableIndexes(refs, "amd64")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
}

func TestVerifyIndexesAcceptsGoodMirror(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64"})
	if err := verifyIndexes(t.Context(), http.DefaultClient, base, "noble", "amd64"); err != nil {
		t.Errorf("verifyIndexes: %v", err)
	}
}

func TestVerifyIndexesRejectsCorruptIndex(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{
		Codename:     "noble",
		Arch:         "amd64",
		CorruptIndex: "universe/binary-amd64/Packages.gz",
	})
	err := verifyIndexes(t.Context(), http.DefaultClient, base, "noble", "amd64")
	if err == nil {
		t.Fatal("verifyIndexes accepted a mirror whose index does not match InRelease")
	}
}

// newStallingMirror serves a minimal archive whose root carries a stale sync
// marker — so screen is obliged to verify it — and whose release file declares
// declaredSize for the single index this tool verifies. The index endpoint
// never finishes: it dribbles one byte at a time for as long as the client
// keeps reading, which is what a hostile or hopelessly slow mirror does. The
// returned counter records how many requests reached that endpoint.
func newStallingMirror(t *testing.T, declaredSize int64) (base string, indexHits *atomic.Int64) {
	t.Helper()
	const indexPath = "main/binary-amd64/Packages.gz"
	hits := &atomic.Int64{}

	inRelease := "Origin: Ubuntu\nSuite: noble\n" +
		"Date: " + time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 MST") + "\n" +
		"SHA256:\n" +
		fmt.Sprintf(" %s %d %s\n", strings.Repeat("0", 64), declaredSize, indexPath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case path == "":
			_, _ = fmt.Fprint(w, `<html><body><pre><a href="Archive-Update-in-Progress-stall">x</a></pre></body></html>`)
		case strings.HasPrefix(path, "Archive-Update-in-Progress-"):
			http.ServeContent(w, r, path, time.Now().Add(-2*time.Hour), strings.NewReader("x"))
		case path == "dists/noble/InRelease":
			_, _ = io.WriteString(w, inRelease)
		case path == "dists/noble/"+indexPath:
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
				if _, err := w.Write([]byte{'a'}); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/", hits
}

// runBounded runs fn and reports whether it finished within limit. It never
// blocks past limit, so a regression that reintroduces an unbounded read fails
// this test instead of hanging the whole test binary.
func runBounded(t *testing.T, limit time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// TestVerifyIndexesRejectsImplausibleDeclaredSize covers the first half of the
// unbounded-read defect: ref.Size comes from the mirror's own release file, so
// a mirror can declare a terabyte and then stream forever. Such a declaration
// must be refused outright — without the index being fetched at all.
func TestVerifyIndexesRejectsImplausibleDeclaredSize(t *testing.T) {
	base, indexHits := newStallingMirror(t, 1_000_000_000_000)

	var err error
	if !runBounded(t, 10*time.Second, func() {
		err = verifyIndexes(t.Context(), http.DefaultClient, base, "noble", "amd64")
	}) {
		t.Fatal("verifyIndexes did not return within 10s: the declared size was trusted and the read is unbounded")
	}
	if err == nil {
		t.Fatal("verifyIndexes accepted an index declaring 1 TB")
	}
	if !strings.Contains(err.Error(), "plausible") {
		t.Errorf("error = %q, want it to say the declared size is implausible", err)
	}
	if got := indexHits.Load(); got != 0 {
		t.Errorf("index was fetched %d times; an implausible size must be rejected before the request", got)
	}
}

// TestScreenBoundsAStallingMirror covers the second half: a plausible declared
// size with a body that never arrives. Screening extends the ordinary request
// timeout to allow large index reads, so its own whole-mirror deadline must
// still bound the work or screenAll's WaitGroup never returns.
func TestScreenBoundsAStallingMirror(t *testing.T) {
	base, _ := newStallingMirror(t, 4<<20) // well inside maxIndexBytes

	// screenBudget is 9x the per-request timeout: 1.8s here, versus a body that
	// would take over two days to dribble out 4 MiB.
	cfg := &config{codename: "noble", arch: "amd64", timeout: 200 * time.Millisecond, maxAge: time.Hour}
	m := &Mirror{URL: base}

	start := time.Now()
	if !runBounded(t, 30*time.Second, func() { screen(t.Context(), http.DefaultClient, cfg, m) }) {
		t.Fatal("screen did not return within 30s: a single stalling mirror can hang the whole run")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("screen took %v, want it bounded by screenBudget (%v)", elapsed, screenBudget(cfg.timeout))
	}
	if m.Err == "" {
		t.Error(`Err = "", want the stalled verification reported as a failure`)
	}
	if m.usable(cfg.maxAge) {
		t.Error("usable() = true, want false for a mirror whose verification never completed")
	}
}

// TestDetectMarkerIgnoresPerConnectionFlapping is the pooled-host case as it
// actually behaves: a DNS or load-balancer pool routes per CONNECTION, not per
// request, so a confirmation that reuses the first request's connection reaches
// the same backend and confirms itself. This server shows the marker only on
// the first connection it accepts; a confirmation on a genuinely new connection
// therefore sees a clean root, and the marker must be discarded.
func TestDetectMarkerIgnoresPerConnectionFlapping(t *testing.T) {
	type connKey struct{}
	var conns atomic.Int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(connKey{}).(int64)
		_, _ = fmt.Fprint(w, `<html><body><pre><a href="dists/">dists/</a>`)
		if id == 1 { // only the first backend carries the marker
			_, _ = fmt.Fprint(w, `<a href="Archive-Update-in-Progress-pool">x</a>`)
		}
		_, _ = fmt.Fprint(w, `</pre></body></html>`)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, conns.Add(1))
	}
	srv.Start()
	t.Cleanup(srv.Close)

	got, err := detectMarker(t.Context(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerNone {
		t.Errorf("State = %v, want markerNone: the confirmation reused the first connection, "+
			"so it reached the same backend and confirmed itself", got.State)
	}
	if n := conns.Load(); n < 2 {
		t.Errorf("server accepted %d connection(s), want at least 2: the confirming request "+
			"must not reuse the connection the first request left in the idle pool", n)
	}
}

// TestVerifyIndexesFallsBackToRelease covers a healthy mirror that publishes
// only the detached Release file. fetchReleaseDate already falls back; before
// this, verification did not, so such a mirror was reported unreachable the
// moment it also carried a marker — a false negative in the safety path.
func TestVerifyIndexesFallsBackToRelease(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64", OnlyRelease: true})
	if err := verifyIndexes(t.Context(), http.DefaultClient, base, "noble", "amd64"); err != nil {
		t.Errorf("verifyIndexes: %v, want a Release-only mirror to verify like an InRelease one", err)
	}
}

func TestParseInReleaseIndexesStopsAtTheNextStanza(t *testing.T) {
	// A real InRelease carries several hash stanzas. Entries from a later one
	// must not be attributed to SHA256, or a mirror could be "verified"
	// against a digest of a different algorithm.
	data := []byte("Origin: Ubuntu\n" +
		"SHA256:\n" +
		" aaaa 12 main/binary-amd64/Packages.gz\n" +
		"\n" +
		" bbbb 34 main/i18n/Translation-en.gz\n" +
		"SHA512:\n" +
		" cccc 56 main/binary-amd64/Packages.gz\n" +
		"Acquire-By-Hash: yes\n")
	refs := parseInReleaseIndexes(data)
	if len(refs) != 2 {
		t.Fatalf("got %d refs %+v, want 2", len(refs), refs)
	}
	for _, r := range refs {
		if r.SHA256 == "cccc" {
			t.Errorf("picked up a SHA512 entry: %+v", r)
		}
	}
	if refs[1].SHA256 != "bbbb" {
		t.Errorf("blank line inside the stanza ended parsing early: %+v", refs)
	}
}

func TestVerifyIndexesRejectsMissingIndex(t *testing.T) {
	// InRelease declares an index the mirror does not actually serve. That is
	// precisely the half-finished dists tree a stale sync lock warns about.
	base := newTestMirror(t, mirrorOpts{
		Codename:     "noble",
		Arch:         "amd64",
		MissingIndex: "universe/binary-amd64/Packages.gz",
	})

	err := verifyIndexes(t.Context(), http.DefaultClient, base, "noble", "amd64")
	if err == nil {
		t.Fatal("verifyIndexes accepted a mirror missing an index InRelease declares")
	}
	if !strings.Contains(err.Error(), "universe") {
		t.Errorf("error does not name the missing index: %v", err)
	}
}

func TestDetectMarkerReportsAnUnconfirmedSighting(t *testing.T) {
	// A pooled host shows the marker on some backends only. The mirror is
	// still treated as clean, but the sighting is recorded so a run can say
	// why, rather than being indistinguishable from a mirror with no marker.
	base := newTestMirror(t, mirrorOpts{Marker: "host-1", MarkerAge: time.Minute, MarkerFlaps: true})
	got, err := detectMarker(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if got.State != markerNone {
		t.Errorf("State = %v, want markerNone", got.State)
	}
	if !got.Unconfirmed {
		t.Error("Unconfirmed = false, want true for a marker seen only once")
	}
	if got.Name == "" {
		t.Error("Name is empty; the sighting should name the file it saw")
	}

	// A mirror with no marker at all must not look like an unconfirmed one.
	clean, err := detectMarker(t.Context(), http.DefaultClient, newTestMirror(t, mirrorOpts{}))
	if err != nil {
		t.Fatalf("detectMarker: %v", err)
	}
	if clean.Unconfirmed || clean.Name != "" {
		t.Errorf("clean mirror reported as %+v", clean)
	}
}
