package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The public -timeout contract covers the response body, not only connection
// setup and headers. A server that flushes headers and then stalls must not be
// able to hold discovery or metadata reads indefinitely.
func TestNewClientBoundsResponseBody(t *testing.T) {
	const timeout = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response writer does not support flushing")
			return
		}
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := newTestClient(timeout).Do(req)
	if err != nil {
		t.Fatalf("request headers: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded, because the handler never ends the body on its own: without
	// this a regression hangs the whole test binary instead of failing here.
	start := time.Now()
	var readErr error
	if !runBounded(t, 10*time.Second, func() { _, readErr = io.ReadAll(resp.Body) }) {
		t.Fatal("body read did not return within 10s: the complete-request timeout is not bounding it")
	}
	if readErr == nil {
		t.Fatal("body read succeeded; want the complete-request timeout to interrupt it")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("body timeout took %v, want well under 1s", elapsed)
	}
}

// Phase 2 must run sequentially: overlapping downloads share the client uplink
// and produce a different winner on every run.
func TestMeasureTopIsSequential(t *testing.T) {
	var mu sync.Mutex
	concurrent, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config{
		codename: "noble", arch: "amd64", probeTime: time.Second,
		timeout: time.Second, probeTop: 3, maxAge: 24 * time.Hour,
	}
	now := time.Now()
	mirrors := []*Mirror{
		{URL: srv.URL + "/a/", Reachable: true, Released: now},
		{URL: srv.URL + "/b/", Reachable: true, Released: now},
		{URL: srv.URL + "/c/", Reachable: true, Released: now},
	}
	measureTop(t.Context(), srv.Client(), cfg, mirrors)

	mu.Lock()
	got := peak
	mu.Unlock()
	// Exactly 1, not just "not greater than 1": this also proves the handler
	// was actually reached, so a measureBandwidth change that errored out
	// before issuing any request could not make this pass vacuously.
	if got != 1 {
		t.Errorf("peak concurrent bandwidth requests = %d, want 1", got)
	}
}

// TestScreenCleanMirrorSkipsVerification pins down the ~31 MB cost guard: a
// mirror with no sync marker must never have its indexes downloaded for
// verification. It counts requests to the specific index paths verifyIndexes
// downloads via a small counting proxy in front of the synthetic mirror,
// rather than touching newTestMirror/mirrorOpts.
func TestScreenCleanMirrorSkipsVerification(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64"}) // no Marker set
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base %q: %v", base, err)
	}
	var indexHits atomic.Int64
	rp := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "Packages.gz") || strings.HasSuffix(r.URL.Path, "Translation-en.gz") {
			indexHits.Add(1)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	cfg := &config{codename: "noble", arch: "amd64", timeout: time.Second, maxAge: time.Hour}
	m := &Mirror{URL: proxy.URL + "/"}
	screen(t.Context(), proxy.Client(), cfg, m)

	if m.Verified {
		t.Error("Verified = true, want false for a clean mirror")
	}
	if m.Demoted {
		t.Error("Demoted = true, want false for a clean mirror")
	}
	if got := indexHits.Load(); got != 0 {
		t.Errorf("index verification requests = %d, want 0 for a clean mirror (cost guard bypassed)", got)
	}
}

// A mirror can serve the archive while refusing to list its root. That does
// not make it unreachable, but it also must not be presented or ranked as if
// the absence of a sync marker had been confirmed.
func TestScreenReportsUnknownMarkerCheckAndDemotesMirror(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/InRelease":
			_, _ = io.WriteString(w, "Origin: Ubuntu\nDate: "+
				time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 MST")+"\n")
		case "/":
			http.Error(w, "directory listing disabled", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config{codename: "noble", arch: "amd64", timeout: time.Second, maxAge: time.Hour}
	unknown := &Mirror{URL: srv.URL + "/"}
	screen(t.Context(), srv.Client(), cfg, unknown)

	if !unknown.Reachable || unknown.Err != "" {
		t.Errorf("mirror should remain usable after only the marker check fails: %+v", unknown)
	}
	if unknown.MarkerStatus != markerUnknown.String() {
		t.Errorf("MarkerStatus = %q, want %q", unknown.MarkerStatus, markerUnknown.String())
	}
	if !unknown.Demoted {
		t.Error("Demoted = false, want unknown marker state ranked below confirmed-clean mirrors")
	}
	if unknown.Verified || unknown.Updating {
		t.Errorf("unknown marker state must not claim a lock or verification: %+v", unknown)
	}
	if !unknown.usable(cfg.maxAge) {
		t.Errorf("usable() = false, want true: mirror=%+v", unknown)
	}

	_, rows := tableRows(t, []*Mirror{unknown})
	if !strings.HasSuffix(rows[0], "marker-unknown") {
		t.Errorf("row = %q, want it to report marker-unknown instead of ok", rows[0])
	}
	clean := &Mirror{URL: "http://clean.example/", Reachable: true, Released: unknown.Released}
	if got := rank([]*Mirror{unknown, clean}, cfg); got[0] != clean {
		t.Errorf("ranked first = %s, want confirmed-clean mirror %s", got[0].URL, clean.URL)
	}
}

// Screening deliberately has a larger budget than an ordinary request because
// a marked mirror may need to stream several indexes. The base client timeout
// must therefore not cut off a valid screening body early.
func TestScreenUsesExtendedBodyBudget(t *testing.T) {
	const timeout = 50 * time.Millisecond
	const bodyDelay = 3 * timeout
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/noble/InRelease":
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("test server response writer does not support flushing")
				return
			}
			flusher.Flush()
			select {
			case <-time.After(bodyDelay):
				_, _ = io.WriteString(w, "Origin: Ubuntu\nDate: "+
					time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 MST")+"\n")
			case <-r.Context().Done():
			}
		case "/":
			_, _ = io.WriteString(w, `<a href="dists/">dists/</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config{codename: "noble", arch: "amd64", timeout: timeout, maxAge: time.Hour}
	m := &Mirror{URL: srv.URL + "/"}
	screen(t.Context(), newTestClient(timeout), cfg, m)
	if m.Err != "" || !m.Reachable {
		t.Errorf("screen rejected a body inside its extended budget: %+v", m)
	}
}

// TestScreenStaleMarkerGoodIndexesVerifiesAndDemotes covers the "leaked lock,
// trustworthy tree" case: verification must succeed and the mirror must be
// demoted rather than excluded (ruling C4), and it must still be usable().
func TestScreenStaleMarkerGoodIndexesVerifiesAndDemotes(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{
		Codename: "noble", Arch: "amd64",
		Marker: "lock", MarkerAge: 2 * time.Hour, // older than staleAfter (1h)
	})
	cfg := &config{codename: "noble", arch: "amd64", timeout: time.Second, maxAge: time.Hour}
	m := &Mirror{URL: base}
	screen(t.Context(), http.DefaultClient, cfg, m)

	if !m.Verified {
		t.Error("Verified = false, want true for a stale-marker mirror with good indexes")
	}
	if !m.Demoted {
		t.Error("Demoted = false, want true for a stale-marker mirror with good indexes")
	}
	if !m.usable(cfg.maxAge) {
		t.Errorf("usable() = false, want true: mirror=%+v", m)
	}
}

// TestScreenMarkerWithCorruptIndexExcludesMirror covers the entire safety
// argument for trusting a marked mirror at all: when the index apt would read
// does not match the hash InRelease declares, the mirror must be excluded,
// not merely demoted.
func TestScreenMarkerWithCorruptIndexExcludesMirror(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{
		Codename: "noble", Arch: "amd64",
		Marker: "lock", MarkerAge: 2 * time.Hour,
		CorruptIndex: "main/binary-amd64/Packages.gz",
	})
	cfg := &config{codename: "noble", arch: "amd64", timeout: time.Second, maxAge: time.Hour}
	m := &Mirror{URL: base}
	screen(t.Context(), http.DefaultClient, cfg, m)

	if m.Err == "" {
		t.Error(`Err = "", want set for a mirror with a corrupt index`)
	}
	if m.Reachable {
		t.Error("Reachable = true, want false for a mirror with a corrupt index")
	}
	if m.usable(cfg.maxAge) {
		t.Error("usable() = true, want false for a mirror with a corrupt index")
	}
}

// TestScreenFreshMarkerSetsUpdating covers ruling C4's requirement that
// Updating stay populated (for JSON output) even though it no longer
// excludes the mirror from usable().
func TestScreenFreshMarkerSetsUpdating(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{
		Codename: "noble", Arch: "amd64",
		Marker: "lock", MarkerAge: time.Minute, // well under staleAfter (1h)
	})
	cfg := &config{codename: "noble", arch: "amd64", timeout: time.Second, maxAge: time.Hour}
	m := &Mirror{URL: base}
	screen(t.Context(), http.DefaultClient, cfg, m)

	if !m.Updating {
		t.Error("Updating = false, want true for a fresh sync-lock marker")
	}
}

func TestMeasureBandwidthUsesContentsAndSkipsWarmup(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64", ContentsSize: 32 << 20})
	cfg := &config{codename: "noble", arch: "amd64", probeBytes: 6 << 20, probeTime: 2 * time.Second, timeout: 10 * time.Second}
	bw, low, err := measureBandwidth(t.Context(), http.DefaultClient, cfg, base)
	if err != nil {
		t.Fatalf("measureBandwidth: %v", err)
	}
	if low {
		t.Error("lowConfidence = true, want false when Contents-<arch>.gz exists")
	}
	if bw <= 0 {
		t.Errorf("bandwidth = %v, want > 0", bw)
	}
}

func TestMeasureBandwidthFallsBackWhenContentsMissing(t *testing.T) {
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64"}) // ContentsSize 0 omits the file
	cfg := &config{codename: "noble", arch: "amd64", probeBytes: 6 << 20, probeTime: 2 * time.Second, timeout: 10 * time.Second}
	_, low, err := measureBandwidth(t.Context(), http.DefaultClient, cfg, base)
	if err != nil {
		t.Fatalf("measureBandwidth: %v", err)
	}
	if !low {
		t.Error("lowConfidence = false, want true when falling back to Packages.gz")
	}
}

// TestMeasureBandwidthExcludesWarmup pins down the exact defect this task
// exists to fix: the timed window must start only after warmupBytes have
// been discarded, not at the response's first byte. The handler stalls for
// warmupDelay before writing anything at all, then writes warmupBytes
// immediately followed by probeBytes with no further delay.
//
//   - With the warm-up discard in place, the clock starts after the stall and
//     after skipping warmupBytes, so it times only the fast probeBytes —
//     which crosses a loopback socket in low single-digit milliseconds — for
//     a rate of hundreds of MB/s or more.
//   - Without the discard (i.e. if timing started at the first byte, or the
//     measured loop consumed warm-up bytes instead of skipping them), the
//     stall falls inside the timed window: the loop stops as soon as it has
//     read probeBytes, which is satisfied by the warm-up burst itself, so
//     elapsed is dominated by warmupDelay and the rate collapses to roughly
//     probeBytes / warmupDelay.
//
// probeBytes (1 MiB) and warmupDelay (300 ms) are sized so the buggy rate is
// at most ~3.5 MB/s while the correct rate is orders of magnitude higher; the
// threshold below (20 MB/s) sits far above the buggy case and far below the
// correct one, so ordinary CI scheduling jitter cannot flip the result.
func TestMeasureBandwidthExcludesWarmup(t *testing.T) {
	const probeBytesSize = 1 << 20
	const warmupDelay = 300 * time.Millisecond
	const minBandwidth = 20 << 20 // 20 MiB/s

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Flush headers before sleeping so client.Do() returns immediately and
		// the delay lands inside the body-read window that timedPull times,
		// rather than being absorbed while waiting for the response headers.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(warmupDelay)
		_, _ = w.Write(bytes.Repeat([]byte{'w'}, warmupBytes))
		_, _ = w.Write(bytes.Repeat([]byte{'p'}, probeBytesSize))
	}))
	t.Cleanup(srv.Close)

	cfg := &config{
		codename: "noble", arch: "amd64",
		probeBytes: probeBytesSize,
		probeTime:  2 * time.Second,
		timeout:    10 * time.Second,
	}
	bw, fellBack, err := timedPull(t.Context(), srv.Client(), cfg, srv.URL+"/")
	if err != nil {
		t.Fatalf("timedPull: %v", err)
	}
	if fellBack {
		t.Error("fellBack = true, want false: the body had a full measured window after the warm-up")
	}
	if bw < minBandwidth {
		t.Errorf("bandwidth = %.0f bytes/s, want >= %d (warm-up not excluded from timed window?)", bw, minBandwidth)
	}
}

// A bandwidth probe has its own budget, which may exceed the base request
// timeout. The client copy used by timedPull must honor that longer budget.
func TestTimedPullUsesBandwidthBudget(t *testing.T) {
	const timeout = 50 * time.Millisecond
	const bodyDelay = 3 * timeout
	const probeBytes = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response writer does not support flushing")
			return
		}
		flusher.Flush()
		select {
		case <-time.After(bodyDelay):
			_, _ = w.Write(bytes.Repeat([]byte{'x'}, warmupBytes+probeBytes))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config{probeBytes: probeBytes, probeTime: time.Second, timeout: timeout}
	bw, low, err := timedPull(t.Context(), newTestClient(timeout), cfg, srv.URL)
	if err != nil {
		t.Fatalf("timedPull: %v", err)
	}
	if low || bw <= 0 {
		t.Errorf("timedPull = (%v, low=%v), want a normal positive measurement", bw, low)
	}
}

// TestMeasureBandwidthExactWarmupBoundary guards a body that ends at
// precisely warmupBytes. A Read can deliver its final bytes together with
// io.EOF, so byte count alone ("discarded < warmupBytes") cannot tell that
// case apart from a full warm-up window with more data still to come; a body
// exactly warmupBytes long must still get the whole-transfer fallback
// measurement rather than the "no data after warm-up" error.
func TestMeasureBandwidthExactWarmupBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{'w'}, warmupBytes))
	}))
	t.Cleanup(srv.Close)

	cfg := &config{codename: "noble", arch: "amd64", probeBytes: 1 << 20, probeTime: 2 * time.Second, timeout: 10 * time.Second}
	bw, fellBack, err := timedPull(t.Context(), srv.Client(), cfg, srv.URL+"/")
	if err != nil {
		t.Fatalf("timedPull: %v, want the whole-transfer fallback measurement instead of an error", err)
	}
	if bw <= 0 {
		t.Errorf("bandwidth = %v, want > 0", bw)
	}
	if !fellBack {
		t.Error("fellBack = false, want true: this figure times the warm-up, not a measured window")
	}
}

// TestMeasureBandwidthFlagsWholeTransferFallbackOnContents covers the case the
// fallback's own comment got wrong: it is reachable on Contents-<arch>.gz too,
// not just on the small Packages.gz. A mirror whose Contents file ends inside
// the warm-up window yields a whole-transfer figure, and that figure must be
// reported low-confidence even though the target was the good one.
func TestMeasureBandwidthFlagsWholeTransferFallbackOnContents(t *testing.T) {
	// Smaller than warmupBytes, so the measured window sees nothing at all.
	base := newTestMirror(t, mirrorOpts{Codename: "noble", Arch: "amd64", ContentsSize: warmupBytes / 4})
	cfg := &config{
		codename: "noble", arch: "amd64", probeBytes: 6 << 20,
		probeTime: 2 * time.Second, timeout: 10 * time.Second,
	}
	bw, low, err := measureBandwidth(t.Context(), http.DefaultClient, cfg, base)
	if err != nil {
		t.Fatalf("measureBandwidth: %v", err)
	}
	if bw <= 0 {
		t.Errorf("bandwidth = %v, want > 0", bw)
	}
	if !low {
		t.Error("lowConfidence = false, want true: the figure came from the whole-transfer fallback")
	}
}

// stalledBody serves warmupBytes followed by tail bytes, but only after
// bodyDelay, with the headers flushed first so the stall lands inside the body
// window timedPull measures rather than being absorbed while waiting for the
// response headers. The delay is what separates the two figures the tests
// below distinguish: the whole-transfer fallback spans it and so reports a few
// MB/s, while a figure timed on the post-warm-up bytes alone reports orders of
// magnitude more, because those bytes are already waiting in the socket buffer.
func stalledBody(t *testing.T, bodyDelay time.Duration, tail int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(bodyDelay)
		_, _ = w.Write(bytes.Repeat([]byte{'w'}, warmupBytes))
		_, _ = w.Write(bytes.Repeat([]byte{'p'}, int(tail)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A transfer that breaks inside the measured window is not a slow mirror, it
// is a failed read, and the bytes that arrived before it broke measure
// nothing. The warm-up loop already rejects a non-EOF read error; the measured
// loop must do the same instead of discarding it and reporting the fragment as
// a successful measurement.
func TestTimedPullFailsOnTransportErrorAfterWarmup(t *testing.T) {
	const tail = 8 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise far more than the handler delivers. The server closes the
		// connection after the short write, so the client's next body read
		// fails with io.ErrUnexpectedEOF rather than reaching a clean end.
		w.Header().Set("Content-Length", strconv.FormatInt(warmupBytes+(1<<20), 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte{'w'}, warmupBytes))
		_, _ = w.Write(bytes.Repeat([]byte{'p'}, tail))
	}))
	t.Cleanup(srv.Close)

	cfg := &config{
		codename: "noble", arch: "amd64", probeBytes: 6 << 20,
		probeTime: 2 * time.Second, timeout: 10 * time.Second,
	}
	bw, low, err := timedPull(t.Context(), srv.Client(), cfg, srv.URL+"/")
	if err == nil {
		t.Fatalf("timedPull = (%v, low=%v, nil), want an error: the transfer broke inside the measured window", bw, low)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("timedPull error = %v, want it to wrap io.ErrUnexpectedEOF: the read error must be preserved, not replaced", err)
	}
}

// A body that ends a few kilobytes after the warm-up hands the measured loop a
// sample that was never transferred at the mirror's pace: it was already in the
// socket buffer, so timing it reports an arbitrary, usually enormous, rate.
// Ranking sorts on the figure alone, so such a sample must not come back as a
// normal measurement; the whole-transfer fallback is the honest figure.
func TestTimedPullFlagsTinySampleAfterWarmup(t *testing.T) {
	const tail = 8 << 10
	const bodyDelay = 300 * time.Millisecond
	// The fallback figure spans bodyDelay: (2 MiB + 8 KiB) / 300 ms is about
	// 7 MB/s, while timing the 8 KiB tail on its own gives hundreds of MB/s.
	const maxBandwidth = 20 << 20

	srv := stalledBody(t, bodyDelay, tail)
	cfg := &config{
		codename: "noble", arch: "amd64", probeBytes: 6 << 20,
		probeTime: 2 * time.Second, timeout: 10 * time.Second,
	}
	bw, low, err := timedPull(t.Context(), srv.Client(), cfg, srv.URL+"/")
	if err != nil {
		t.Fatalf("timedPull: %v, want the whole-transfer fallback measurement", err)
	}
	if !low {
		t.Error("lowConfidence = false, want true: the measured window ended before it measured anything")
	}
	if bw >= maxBandwidth {
		t.Errorf("bandwidth = %.0f bytes/s, want < %d: the tiny post-warm-up sample must not be timed on its own", bw, maxBandwidth)
	}
}

// The converse guard: a window cut short only slightly still timed a real
// transfer, so it must stay a normal measurement. Replacing it with the
// whole-transfer fallback would fold the slow-start ramp the warm-up exists to
// exclude back into the figure and demote an honest mirror.
func TestTimedPullAcceptsSubstantialTruncatedWindow(t *testing.T) {
	const probeBytes = 4 << 20
	const tail = 3 << 20 // three quarters of the byte budget, then a clean EOF
	const bodyDelay = 500 * time.Millisecond
	// The fallback figure would be (2 MiB + 3 MiB) / 500 ms, about 10 MB/s,
	// while the measured window alone crosses loopback at far more than this.
	const minBandwidth = 20 << 20

	srv := stalledBody(t, bodyDelay, tail)
	cfg := &config{
		codename: "noble", arch: "amd64", probeBytes: probeBytes,
		probeTime: 2 * time.Second, timeout: 10 * time.Second,
	}
	bw, low, err := timedPull(t.Context(), srv.Client(), cfg, srv.URL+"/")
	if err != nil {
		t.Fatalf("timedPull: %v", err)
	}
	if low {
		t.Error("lowConfidence = true, want false: the window covered most of the byte budget before the body ended")
	}
	if bw < minBandwidth {
		t.Errorf("bandwidth = %.0f bytes/s, want >= %d: a nearly complete window must not be replaced by the whole-transfer figure", bw, minBandwidth)
	}
}

// TestMeasureTopSkipsStaleCandidates pins down why Behind is computed between
// the phases: -probe-top slots are scarce, and a stale mirror that ranking will
// reject must not take one. With probeTop 1 and the stale mirror listed first,
// the measured mirror must be the fresh one.
func TestMeasureTopSkipsStaleCandidates(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0])
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config{
		codename: "noble", arch: "amd64", probeTime: time.Second,
		timeout: time.Second, probeTop: 1, maxAge: 24 * time.Hour,
	}
	stale := &Mirror{URL: srv.URL + "/stale/", Reachable: true, Released: time.Now(), Behind: 72 * time.Hour}
	fresh := &Mirror{URL: srv.URL + "/fresh/", Reachable: true, Released: time.Now()}
	measureTop(t.Context(), srv.Client(), cfg, []*Mirror{stale, fresh})

	mu.Lock()
	got := append([]string{}, hits...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no bandwidth requests were made at all")
	}
	for _, h := range got {
		if h == "stale" {
			t.Fatalf("a mirror %v behind consumed a -probe-top slot; requests = %v", 72*time.Hour, got)
		}
	}
}

func TestShortErr(t *testing.T) {
	const target = "https://ftp.uni-stuttgart.de/ubuntu/dists/resolute/Release"
	wrap := func(inner error) error {
		return &url.Error{Op: "Get", URL: target, Err: inner}
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			// The reported case: the whole request URL swamped the column.
			name: "tls handshake timeout",
			err:  wrap(errors.New("net/http: TLS handshake timeout")),
			want: "TLS handshake timeout",
		},
		{"deadline", wrap(context.DeadlineExceeded), "timed out"},
		{"refused", wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), "connection refused"},
		{"dns", wrap(&net.DNSError{Err: "no such host", Name: "nope.invalid"}), "DNS lookup failed"},
		{"already short", errors.New("HTTP 403"), "HTTP 403"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shortErr(c.err)
			if got != c.want {
				t.Errorf("shortErr() = %q, want %q", got, c.want)
			}
			if strings.Contains(got, target) {
				t.Errorf("shortErr() still carries the request URL: %q", got)
			}
		})
	}
}

func TestSweepResponseProbesEachMirrorThreeTimes(t *testing.T) {
	// The three probes absorb a slow DNS answer or a momentary failure, so the
	// count is load-bearing rather than incidental. By default probes are
	// cache-friendly: same URL every time, no busting query, and a target a
	// front end will actually serve from cache.
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("cf-cache-status", "HIT")
		_, _ = w.Write([]byte("Origin: Ubuntu\n"))
	}))
	t.Cleanup(srv.Close)

	m := &Mirror{URL: srv.URL + "/"}
	cfg := &config{codename: "noble", arch: "amd64", concurrency: 4, timeout: 5 * time.Second}
	sweepLatency(t.Context(), srv.Client(), cfg, []*Mirror{m})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("server saw %d requests %v, want 3", len(got), got)
	}
	if m.Response <= 0 {
		t.Errorf("Response = %v, want a positive measurement", m.Response)
	}
	for _, u := range got {
		if strings.Contains(u, "aptmir=") {
			t.Errorf("probe %q busted the cache; caching is allowed by default", u)
		}
		// Release is never cached by a front end that caches by extension, so
		// probing it would measure the cold path and defeat the default.
		if !strings.Contains(u, "Packages.gz") {
			t.Errorf("probe %q is not a cacheable target", u)
		}
	}
	if m.CDN != "cloudflare" {
		t.Errorf("CDN = %q, want cloudflare detected from the response header", m.CDN)
	}
}

func TestSweepResponseBustsTheCacheUnderNoCache(t *testing.T) {
	// -no-cache measures the cold path, so every probe must reach the origin.
	// That is also what keeps best-of-three honest: with a cacheable target the
	// first probe warms the edge and the rest are served from it, so taking the
	// best would report a cache entry this tool created.
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.RequestURI())
		mu.Unlock()
		_, _ = w.Write([]byte("Origin: Ubuntu\n"))
	}))
	t.Cleanup(srv.Close)

	m := &Mirror{URL: srv.URL + "/"}
	cfg := &config{codename: "noble", arch: "amd64", concurrency: 4,
		timeout: 5 * time.Second, noCache: true}
	sweepLatency(t.Context(), srv.Client(), cfg, []*Mirror{m})

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, u := range got {
		if !strings.Contains(u, "/dists/noble/Release") {
			t.Errorf("probed %q, want the detached Release file", u)
		}
		if !strings.Contains(u, "aptmir=") {
			t.Errorf("probe %q carries no cache-busting query", u)
		}
		if seen[u] {
			t.Errorf("probe URL %q repeated; the cache key must differ each time", u)
		}
		seen[u] = true
	}
}

func TestCDNFrom(t *testing.T) {
	for header, want := range map[string]string{
		"cf-cache-status": "cloudflare",
		"x-served-by":     "fastly",
		"x-varnish":       "varnish",
		"x-amz-cf-pop":    "cloudfront",
	} {
		h := http.Header{}
		h.Set(header, "something")
		if got := cdnFrom(h); got != want {
			t.Errorf("cdnFrom(%s) = %q, want %q", header, got, want)
		}
	}
	if got := cdnFrom(http.Header{}); got != "" {
		t.Errorf("cdnFrom(no headers) = %q, want empty", got)
	}
}

func TestNearestKeepsTheClosestAndDropsDeadOnes(t *testing.T) {
	dead := &Mirror{URL: "http://dead/"} // no probe answered
	far := &Mirror{URL: "http://far/", Response: 900 * time.Millisecond}
	near := &Mirror{URL: "http://near/", Response: 10 * time.Millisecond}
	mid := &Mirror{URL: "http://mid/", Response: 80 * time.Millisecond}

	got := nearest([]*Mirror{dead, far, near, mid}, 2)
	if len(got) != 2 || got[0] != near || got[1] != mid {
		t.Fatalf("nearest(2) = %v, want [near mid]", got)
	}
	// A host that answered nothing must never outrank one that did.
	all := nearest([]*Mirror{dead, near}, 0)
	if all[0] != near || all[1] != dead {
		t.Errorf("nearest(0) put the dead host first: %v", all)
	}
	if len(nearest([]*Mirror{near, mid}, 0)) != 2 {
		t.Error("nearest(0) should keep every candidate")
	}
}

func TestMeasureBandwidthPrimesTheCacheWhenCachingIsAllowed(t *testing.T) {
	// -allow-caching has to reach this phase too. If it only relaxed the sweep,
	// a CDN-fronted mirror would pass the cut and then be measured on its cold
	// path, lose anyway, and the flag would appear to do nothing.
	var mu sync.Mutex
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		fetches++
		mu.Unlock()
		_, _ = w.Write(bytes.Repeat([]byte("a"), 9<<20))
	}))
	t.Cleanup(srv.Close)

	base := func(allow bool) *config {
		return &config{
			codename: "noble", arch: "amd64", probeBytes: 2 << 20,
			probeTime: 5 * time.Second, timeout: 5 * time.Second, noCache: !allow,
		}
	}
	if _, _, err := measureBandwidth(t.Context(), srv.Client(), base(false), srv.URL+"/"); err != nil {
		t.Fatalf("measureBandwidth: %v", err)
	}
	mu.Lock()
	cold := fetches
	mu.Unlock()
	if cold != 1 {
		t.Errorf("cold path made %d fetches, want 1", cold)
	}

	if _, _, err := measureBandwidth(t.Context(), srv.Client(), base(true), srv.URL+"/"); err != nil {
		t.Fatalf("measureBandwidth: %v", err)
	}
	mu.Lock()
	warm := fetches - cold
	mu.Unlock()
	if warm != 2 {
		t.Errorf("-allow-caching made %d fetches, want 2 (prime then measure)", warm)
	}
}

func TestSweepReportsTheFinalCountOnItsOwnLine(t *testing.T) {
	// The ticker samples once a second, so the last mirrors finish after its
	// final tick: the display used to stop at 52/53. And the in-place updates
	// end in a carriage return, so without a closing line the next message ran
	// straight on from this one in captured output.
	var buf bytes.Buffer
	var done atomic.Int64
	done.Store(53)
	stop := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		reportProgress(&buf, "measured", &done, 53, stop)
	}()
	close(stop)
	<-reported

	got := buf.String()
	if !strings.Contains(got, "measured 53/53") {
		t.Errorf("closing line = %q, want the full count", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("closing line %q does not end in a newline", got)
	}
}

func TestSweepLatencyWaitsForItsReporter(t *testing.T) {
	// sweepLatency and the caller's next message share a stream, so the sweep
	// must not return while the reporter still has a line to write.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Origin: Ubuntu\n"))
	}))
	t.Cleanup(srv.Close)

	m := &Mirror{URL: srv.URL + "/"}
	cfg := &config{codename: "noble", concurrency: 2, timeout: 5 * time.Second}
	// Run under -race repeatedly: an unsynchronised reporter writing to stderr
	// after the sweep returns is what this pins down.
	for range 5 {
		sweepLatency(t.Context(), srv.Client(), cfg, []*Mirror{m})
	}
	if m.Response <= 0 {
		t.Errorf("Response = %v, want a measurement", m.Response)
	}
}

func TestProgressLabelsEachPhase(t *testing.T) {
	// The sweep and the bandwidth phase share one reporter, so the label is
	// what tells the user which of them is running.
	for label, want := range map[string]string{
		"measured":              "measured 2/2\n",
		"screened":              "screened 2/2\n",
		"measured bandwidth on": "measured bandwidth on 2/2\n",
	} {
		var buf bytes.Buffer
		var done atomic.Int64
		done.Store(2)
		stop := make(chan struct{})
		reported := make(chan struct{})
		go func() {
			defer close(reported)
			reportProgress(&buf, label, &done, 2, stop)
		}()
		close(stop)
		<-reported
		if got := buf.String(); got != want {
			t.Errorf("label %q produced %q, want %q", label, got, want)
		}
	}
}

func TestTrackProgressIsSilentUnderDebug(t *testing.T) {
	// -d traces every probe and measurement individually, so a once-a-second
	// count of them adds nothing and obscures the detail.
	var buf bytes.Buffer
	prevOut := progressOut
	progressOut = &buf
	t.Cleanup(func() { progressOut = prevOut; disableDebug() })

	enableDebug(io.Discard)
	var done atomic.Int64
	done.Store(7)
	trackProgress("measured", &done, 7)()
	if buf.Len() != 0 {
		t.Errorf("progress wrote %q under -d", buf.String())
	}

	// With -d off the closing line still reports the full count.
	disableDebug()
	trackProgress("measured", &done, 7)()
	if got := buf.String(); got != "measured 7/7\n" {
		t.Errorf("progress wrote %q, want the closing count", got)
	}
}
