package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func get(ctx context.Context, client *http.Client, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		debugLog.Debug("request failed", "method", "GET", "url", u, "err", shortErr(err))
		return nil, err
	}
	debugLog.Debug("request", "method", "GET", "url", u,
		"status", resp.StatusCode, "dur", time.Since(start))
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// headOK reports whether a HEAD request for u is answered with 200 OK.
func headOK(ctx context.Context, client *http.Client, u string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		debugLog.Debug("request failed", "method", "HEAD", "url", u, "err", shortErr(err))
		return false, err
	}
	debugLog.Debug("request", "method", "HEAD", "url", u,
		"status", resp.StatusCode, "dur", time.Since(start))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// ---------------------------------------------------------------- probing

// sweepTimeout bounds a single sweep probe. It is deliberately far shorter than
// -timeout: a mirror that cannot return a first byte within three seconds is
// never going to win, and the sweep visits every candidate three times.
const sweepTimeout = 3 * time.Second

// cdnHeaders are the response headers that betray a caching front end, keyed
// by the vendor they identify. Detection is free: the sweep already has the
// response in hand, so no extra request is needed to classify a mirror.
var cdnHeaders = []struct {
	header, vendor string
}{
	{"cf-cache-status", "cloudflare"},
	{"x-served-by", "fastly"},
	{"x-cache", "cache"},
	{"x-varnish", "varnish"},
	{"x-amz-cf-pop", "cloudfront"},
}

// cdnFrom names the caching front end a response came through, or "".
func cdnFrom(h http.Header) string {
	for _, c := range cdnHeaders {
		if h.Get(c.header) != "" {
			return c.vendor
		}
	}
	return ""
}

// responseTime measures time to first byte for one probe, and reports any
// caching front end the response passed through.
//
// The target is the detached Release file, which apt itself fetches and which
// carries no extension, so a front end that caches by extension leaves it
// alone. Unless the caller allows caching, a unique query is appended as well,
// which gives a distinct cache key even where the extension rule does not
// apply. Only the first byte is read; the body is abandoned immediately.
//
// This replaced a TCP handshake, which measured the distance to the nearest
// edge rather than to the mirror: a Hong Kong mirror behind Cloudflare
// answered a handshake in 14 ms while its first byte took 691 ms, and on that
// basis it displaced genuinely fast mirrors out of the screening cut entirely.
func responseTime(ctx context.Context, client *http.Client, cfg *config, base string) (time.Duration, string, error) {
	// The target depends on which path is being measured, and the two pull in
	// opposite directions.
	//
	// By default we want the cold path a single machine pays, so we probe the
	// detached Release file: it has no extension, so a front end caching by
	// extension leaves it alone, and a unique query gives a distinct cache key
	// even where that rule does not apply. Measured on a Cloudflare-fronted
	// mirror, Release comes back DYNAMIC every time — never cached.
	//
	// Under -allow-caching we want the warm path a fleet sees, which needs a
	// target the edge will actually serve from cache. Release cannot do that
	// however we request it, so the probe moves to Packages.gz: on the same
	// mirror that returns HIT at 53-63 ms against 255-681 ms for Release.
	u := base + "dists/" + cfg.codename + "/Release"
	if cfg.cachingAllowed() {
		u = base + "dists/" + cfg.codename + "/main/binary-" + cfg.arch + "/Packages.gz"
	} else {
		u += fmt.Sprintf("?aptmir=%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	// One byte is all the measurement needs, and it keeps the sweep's cost to
	// the round trip rather than the file.
	req.Header.Set("Range", "bytes=0-0")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return 0, cdnFrom(resp.Header), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var b [1]byte
	if _, err := resp.Body.Read(b[:]); err != nil && !errors.Is(err, io.EOF) {
		return 0, cdnFrom(resp.Header), err
	}
	return time.Since(start), cdnFrom(resp.Header), nil
}

// sweepResponse measures one mirror with up to three probes, keeping the best.
//
// Best-of-three is only sound because every probe reaches the origin: with a
// cacheable target the first probe warms the edge and the rest are served from
// it, so picking the best would deliberately report the cache. Measured on a
// Cloudflare-fronted mirror, three probes of a .gz went 675 ms, 57 ms, 48 ms.
// If you ever make this target cacheable, best-of-N stops being valid.
func sweepResponse(ctx context.Context, client *http.Client, cfg *config, m *Mirror) {
	best := time.Duration(0)
	for i := range 3 {
		d, cdn, err := responseTime(ctx, client, cfg, m.URL)
		if cdn != "" {
			m.CDN = cdn
		}
		if err != nil {
			// A server strict about unknown query strings would reject the
			// cache-busting probe; fall back once so it is not read as dead.
			if i == 0 && !cfg.cachingAllowed() {
				relaxed := *cfg
				relaxed.noCache = false
				if d2, cdn2, err2 := responseTime(ctx, client, &relaxed, m.URL); err2 == nil {
					if cdn2 != "" {
						m.CDN = cdn2
					}
					best = d2
				}
			}
			continue
		}
		// The cdn key is omitted rather than logged empty: most mirrors have
		// no front end, and cdn="" on every probe is noise in a long trace.
		probe := []any{"mirror", m.URL, "ttfb", d}
		if cdn != "" {
			probe = append(probe, "cdn", cdn)
		}
		debugLog.Debug("probe", probe...)
		if best == 0 || d < best {
			best = d
		}
	}
	debugLog.Debug("response", "mirror", m.URL, "best_of", 3, "ttfb", best)
	m.Response = best
}

// sweepLatency measures every candidate's response time, concurrently.
//
// Each host gets three probes rather than one. They are not only about
// measuring more accurately: they absorb a slow DNS answer or a momentary
// network hiccup that a single probe would record as unreachable. Measured
// against the real list, one probe classifies 33-59 hosts as dead where three
// classify 13-24, so roughly 20-40 live mirrors per run would otherwise be
// discarded before anything else looked at them. That is worth the extra
// minute: a slower correct answer beats a quick wrong one.
func sweepLatency(ctx context.Context, client *http.Client, cfg *config, mirrors []*Mirror) {
	var done atomic.Int64
	defer trackProgress("measured", &done, len(mirrors))()

	sem := make(chan struct{}, cfg.concurrency)
	var wg sync.WaitGroup
	for _, m := range mirrors {
		wg.Add(1)
		go func(m *Mirror) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sweepResponse(ctx, client, cfg, m)
			done.Add(1)
		}(m)
	}
	wg.Wait()
}

// progressOut is where phase progress goes, replaceable so tests can read it.
var progressOut io.Writer = os.Stderr

// trackProgress starts a once-a-second reporter and returns the function that
// stops it. That function waits for the reporter's closing line before
// returning, because the reporter and the caller's next message share a stream.
func trackProgress(label string, done *atomic.Int64, total int) func() {
	// Under -d the trace already reports every probe, verification and
	// measurement as it happens, so a once-a-second count of them is noise laid
	// over the detail it summarises.
	if debugging() {
		return func() {}
	}
	stop := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		reportProgress(progressOut, label, done, total, stop)
	}()
	return func() {
		close(stop)
		<-reported
	}
}

// reportProgress ticks out a phase's progress once a second. The sweep and the
// bandwidth measurement are the two long phases, and silence in either is what
// makes the tool look hung.
func reportProgress(w io.Writer, label string, done *atomic.Int64, total int, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			finishProgress(w, label, done.Load(), total)
			return
		case <-tick.C:
			if sweepOnTerminal() {
				_, _ = fmt.Fprintf(w, "\r%s %d/%d...", label, done.Load(), total)
			} else {
				_, _ = fmt.Fprintf(w, "%s %d/%d...\n", label, done.Load(), total)
			}
		}
	}
}

// finishSweep replaces the in-place progress with a completed line.
//
// It exists for two reasons. The ticker samples once a second, so the last
// mirrors finish after its final tick and the display stops short of the total
// — it reported 52/53 on a 53-candidate run. And the in-place updates end in a
// carriage return rather than a newline: erasing them looks right on a live
// terminal but leaves captured output running the next message straight on to
// the end of this one. A real closing line fixes both.
func finishProgress(w io.Writer, label string, done int64, total int) {
	if sweepOnTerminal() {
		_, _ = fmt.Fprintf(w, "\r%*s\r", 40, "")
	}
	_, _ = fmt.Fprintf(w, "%s %d/%d\n", label, done, total)
}

// sweepOnTerminal reports whether stderr is a terminal, so progress rewrites a
// single line there and stays readable as plain lines when it is redirected.
func sweepOnTerminal() bool {
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// nearest keeps the n lowest-latency candidates and drops the rest entirely, so
// the table reports only on mirrors that were actually examined. A candidate
// that answered no probe at all has zero latency and sorts last.
func nearest(mirrors []*Mirror, n int) []*Mirror {
	out := make([]*Mirror, len(mirrors))
	copy(out, mirrors)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Response, out[j].Response
		switch {
		case a == 0:
			return false
		case b == 0:
			return true
		default:
			return a < b
		}
	})
	if n > 0 && len(out) > n {
		debugLog.Debug("screening cut", "keeping", n, "of", len(out),
			"slowest_kept", out[n-1].Response)
		out = out[:n]
	}
	return out
}

// screenAll checks freshness and sync-marker state concurrently, for the
// -screen-top closest candidates only. Response is already known by this point:
// sweepLatency measured every candidate, and that is what decided which few
// reach this far.
//
// These checks are cheap, but the phase is not uniformly cheap: a mirror that
// shows a sync marker also has its indexes verified here, which downloads
// roughly 31 MB from that mirror. Verification is the one expensive thing this
// phase can do, and up to -concurrency of them can be in flight at once. It
// stays here because it is a safety gate: a marked mirror must not reach the
// ranking at all until its tree has been checked.
func screenAll(ctx context.Context, client *http.Client, cfg *config, mirrors []*Mirror) []*Mirror {
	// Screening looks quick per mirror, but a marked one has its indexes
	// verified here, which pulls roughly 31 MB from that mirror alone. A few of
	// those and the phase sits silent for a long time.
	var done atomic.Int64
	defer trackProgress("screened", &done, len(mirrors))()

	sem := make(chan struct{}, cfg.concurrency)
	var wg sync.WaitGroup
	for _, m := range mirrors {
		wg.Add(1)
		go func(m *Mirror) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			screen(ctx, client, cfg, m)
			done.Add(1)
		}(m)
	}
	wg.Wait()
	return mirrors
}

// screenBudgetFactor turns the base request timeout into a whole-of-screening
// budget for a single mirror. Screening may download roughly 31 MB across
// several indexes, so the ordinary 10 s body timeout would reject a valid but
// slow mirror. Giving both the client and the enclosing context 90 s covers
// about 350 kB/s, far below any mirror worth ranking, while still guaranteeing
// that no single mirror can stall the whole run.
const screenBudgetFactor = 9

// screenBudget is how long one mirror gets for the whole of phase 1.
func screenBudget(timeout time.Duration) time.Duration {
	return time.Duration(screenBudgetFactor) * timeout
}

// screen fills in everything measurable without a large download, except for
// the index verification a sync marker forces. It gives itself a deadline so a
// mirror that stops making progress mid-body cannot hold up screenAll.
func screen(ctx context.Context, client *http.Client, cfg *config, m *Mirror) {
	m.Pool = isPoolHost(m.URL)

	budget := screenBudget(cfg.timeout)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client = clientWithTimeout(client, budget)

	date, err := fetchReleaseDate(ctx, client, cfg, m.URL)
	if err != nil {
		m.Err = shortErr(err)
		return
	}
	m.Reachable = true
	m.Released = date

	info, err := detectMarker(ctx, client, m.URL)
	if err != nil {
		// The archive root could not be read. A 403 or 404 is the common
		// case, wherever autoindex is off, but a timeout or a reset root
		// request reads the same way: all of them leave the sync-lock state
		// unknown. Keep the mirror usable, but rank it below mirrors whose
		// roots were checked successfully.
		m.MarkerStatus = markerUnknown.String()
		m.Demoted = true
		debugLog.Debug("marker check failed", "mirror", m.URL, "err", shortErr(err))
		return
	}
	m.MarkerStatus = info.State.String()
	m.Updating = info.State == markerFresh
	if info.State == markerNone {
		if info.Unconfirmed {
			debugLog.Debug("marker unconfirmed", "mirror", m.URL, "marker", info.Name)
		}
		return
	}
	debugLog.Debug("marker", "mirror", m.URL, "state", info.State.String(),
		"marker", info.Name, "modified", markerAge(info))
	// A marker means the tree may be mid-mutation, which freshness cannot see.
	// Verify the indexes apt reads before trusting the mirror at all.
	if err := verifyIndexes(ctx, client, m.URL, cfg.codename, cfg.arch); err != nil {
		m.Err = "index verification failed: " + err.Error()
		m.Reachable = false
		return
	}
	m.Verified = true
	m.Demoted = true
}

// measureTop measures bandwidth on the best candidates, one at a time. Running
// these concurrently makes them share the uplink and randomises the ranking.
//
// Only mirrors that are usable are eligible: the -probe-top slots are few, and
// measuring a mirror that ranking will reject for staleness spends one of them
// on a number nobody will act on. This is why Behind is computed between the
// two phases rather than after both — usable() cannot answer before it is.
func measureTop(ctx context.Context, client *http.Client, cfg *config, mirrors []*Mirror) []*Mirror {
	candidates := make([]*Mirror, 0, len(mirrors))
	for _, m := range mirrors {
		if m.usable(cfg.maxAge) {
			candidates = append(candidates, m)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Demoted != b.Demoted {
			return !a.Demoted
		}
		if a.Response == 0 {
			return false
		}
		if b.Response == 0 {
			return true
		}
		return a.Response < b.Response
	})
	if cfg.probeTop > 0 && len(candidates) > cfg.probeTop {
		candidates = candidates[:cfg.probeTop]
	}
	var done atomic.Int64
	stopProgress := trackProgress("measured bandwidth on", &done, len(candidates))
	defer stopProgress()

	debugLog.Debug("bandwidth phase", "measuring", len(candidates), "usable", len(mirrors))
	for _, m := range candidates {
		bw, low, err := measureBandwidth(ctx, client, cfg, m.URL)
		done.Add(1)
		if err != nil {
			debugLog.Debug("bandwidth failed", "mirror", m.URL, "err", shortErr(err))
			continue
		}
		debugLog.Debug("bandwidth", "mirror", m.URL, "rate", formatRate(bw), "low_confidence", low)
		m.Bandwidth = bw
		m.LowConfidence = low
	}
	return mirrors
}

// shortErr renders a transport failure as a phrase fit for the STATUS column.
// A bare Error() from net/http repeats the whole request URL, which the column
// already shows in the MIRROR cell, so the url.Error wrapper is peeled off and
// the failures worth naming are named.
//
// Only the leaf transport error goes through here. Index-verification failures
// keep their full text, because the mismatching path and hash are the entire
// diagnostic value of those.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed"
	}
	// net/http prefixes its own transport failures, "TLS handshake timeout"
	// among them, and the prefix tells the reader nothing.
	return strings.TrimPrefix(err.Error(), "net/http: ")
}

var dateFieldRe = regexp.MustCompile(`(?m)^Date:\s*(.+)$`)

// fetchReleaseDate reads the Date: stanza from InRelease, falling back to the
// detached Release file for mirrors that do not publish InRelease.
func fetchReleaseDate(ctx context.Context, client *http.Client, cfg *config, base string) (time.Time, error) {
	var lastErr error
	for _, name := range []string{"InRelease", "Release"} {
		u := base + "dists/" + cfg.codename + "/" + name
		body, err := get(ctx, client, u)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(body, 256<<10))
		_ = body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		match := dateFieldRe.FindSubmatch(data)
		if match == nil {
			lastErr = fmt.Errorf("no Date field in %s", name)
			continue
		}
		t, err := parseReleaseTime(strings.TrimSpace(string(match[1])))
		if err != nil {
			lastErr = err
			continue
		}
		debugLog.Debug("release", "mirror", base, "file", name, "date", t)
		return t, nil
	}
	if lastErr == nil {
		lastErr = errors.New("unavailable")
	}
	return time.Time{}, lastErr
}

func parseReleaseTime(s string) (time.Time, error) {
	layouts := []string{
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
		time.RFC1123Z,
		time.RFC1123,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse Date %q", s)
}

const (
	// warmupBytes is discarded before the clock starts. A 1.79 MB transfer at
	// 16 ms RTT is almost entirely TCP slow-start, which measures how fast the
	// congestion window opened rather than what the mirror can sustain.
	warmupBytes = 2 << 20
	// measureBytes is the default size of the timed measurement window: the
	// default value of the -probe-bytes flag, which cfg.probeBytes holds.
	measureBytes = 6 << 20
	// materialWindowShare is how much of either measurement budget a window
	// cut short by EOF must still have covered for its figure to count as a
	// normal measurement. A body ending just shy of the budget was timed over
	// enough of a transfer to mean something; one ending just past the warm-up
	// was not timed so much as clocked draining the socket buffer.
	materialWindowShare = 0.5
)

// measureBandwidth reports sustained throughput in bytes per second. It pulls a
// bounded Range from Contents-<arch>.gz, which is large enough that the timed
// window sits past slow-start, and reports lowConfidence when it had to fall
// back to the much smaller Packages.gz.
func measureBandwidth(ctx context.Context, client *http.Client, cfg *config, base string) (float64, bool, error) {
	dists := base + "dists/" + cfg.codename + "/"
	targets := []struct {
		url string
		low bool
	}{
		{dists + "Contents-" + cfg.arch + ".gz", false},
		{dists + "main/binary-" + cfg.arch + "/Packages.gz", true},
	}

	var lastErr error
	for _, target := range targets {
		// Under -allow-caching the figure that matters is the warm one a fleet
		// sees, so prime the edge first and measure the second fetch. Without
		// this the flag would relax the sweep and then still report the cold
		// path here, so a CDN-fronted mirror would lose anyway and the flag
		// would look broken. Measured on one such mirror: 5.6 MB/s cold,
		// 43-51 MB/s warm.
		if cfg.cachingAllowed() {
			_, _, _ = timedPull(ctx, client, cfg, target.url)
		}
		bw, fellBack, err := timedPull(ctx, client, cfg, target.url)
		if err == nil {
			// Either reason is enough to distrust the figure: a small target,
			// or a measurement that had to time the warm-up because the body
			// ended inside it, or too soon after it for the measured window to
			// have measured anything. The latter is reachable on Contents-<arch>.gz
			// too — a mirror serving a truncated or tiny Contents file lands
			// there — so it cannot be inferred from the target alone.
			return bw, target.low || fellBack, nil
		}
		lastErr = err
	}
	return 0, false, lastErr
}

// timedPull downloads warmupBytes, discards them, then times the next window.
// The second result reports that it could not do that and timed the whole
// transfer instead, which makes the figure low-confidence whatever the target.
// A read that fails rather than ends, in either window, is returned as an
// error: the bytes that arrived before it broke measure nothing.
func timedPull(ctx context.Context, client *http.Client, cfg *config, u string) (float64, bool, error) {
	budget := cfg.probeTime + 2*cfg.timeout
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client = clientWithTimeout(client, budget)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", warmupBytes+cfg.probeBytes-1))
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	buf := make([]byte, 64<<10)
	warmupStart := time.Now()
	var discarded int64
	for discarded < warmupBytes {
		chunk := min(warmupBytes-discarded, int64(len(buf)))
		n, rerr := resp.Body.Read(buf[:chunk])
		discarded += int64(n)
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return 0, false, fmt.Errorf("warm-up: %w", rerr)
			}
			// Reached EOF at or before warmupBytes: nothing left for a
			// separate measured window. Fall through to the measured loop
			// below anyway — its own zero-byte result is what triggers the
			// whole-transfer fallback, since a Reader may signal EOF only on
			// a later, empty Read rather than together with its final bytes,
			// so byte count alone cannot always tell "short" from "exact"
			// here.
			break
		}
	}

	deadline := time.Now().Add(cfg.probeTime)
	var total int64
	start := time.Now()
	var truncated bool
	for total < cfg.probeBytes && time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		total += int64(n)
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				// The transfer broke rather than ended, exactly as the
				// warm-up loop above treats the same failure. Whatever
				// arrived before it broke is a fragment of an interrupted
				// window, not a measurement of the mirror.
				return 0, false, fmt.Errorf("measured window: %w", rerr)
			}
			truncated = true
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	// A window that ran out either budget — probeBytes read, or probeTime
	// elapsed — timed what it was meant to. One cut short by EOF only counts
	// if it still covered a material share of one of them: a body ending a
	// few kilobytes past the warm-up leaves a sample that was sitting in the
	// socket buffer rather than crossing the wire, so timing it reports an
	// arbitrary and usually enormous rate. Ranking sorts on the figure alone,
	// so such a sample would outrank every honestly measured mirror.
	measured := !truncated ||
		float64(total) >= materialWindowShare*float64(cfg.probeBytes) ||
		elapsed >= materialWindowShare*cfg.probeTime.Seconds()
	if total > 0 && measured {
		debugLog.Debug("pull", "url", u, "timed_bytes", total, "warmup_bytes", discarded, "truncated", truncated)
		if elapsed <= 0 {
			return 0, false, errors.New("no data after warm-up")
		}
		return float64(total) / elapsed, false, nil
	}

	// The measured phase produced nothing worth timing: the source ended at or
	// before warmupBytes, whether or not the discard loop above happened to
	// observe that EOF directly, or so soon after it that the sample above was
	// rejected. Time the whole transfer instead of failing outright, and say
	// so: this figure includes the slow-start ramp the warm-up exists to
	// exclude, so it is low-confidence whichever target produced it. The small
	// Packages.gz fallback is the expected way to land here, but a truncated
	// or unexpectedly tiny Contents-<arch>.gz lands here too, which is why the
	// caller cannot infer it from the target alone.
	elapsed = time.Since(warmupStart).Seconds()
	if elapsed <= 0 || discarded+total == 0 {
		return 0, false, errors.New("no data")
	}
	return float64(discarded+total) / elapsed, true, nil
}
