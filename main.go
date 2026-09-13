// Command aptmir ranks Ubuntu archive mirrors by measured freshness and
// throughput, and can rewrite the local apt sources to use the best one.
//
// It understands both the legacy one-line sources format and the deb822
// format that Ubuntu adopted as the default in 24.04, so it works on 22.04
// through 26.04 and later without special-casing individual releases.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultArchive   = "http://archive.ubuntu.com/ubuntu/"
	defaultPorts     = "http://ports.ubuntu.com/ubuntu-ports/"
	oldReleases      = "http://old-releases.ubuntu.com/ubuntu/"
	securityHost     = "security.ubuntu.com"
	geoMirrorList    = "http://mirrors.ubuntu.com/mirrors.txt"
	countryMirrorFmt = "http://mirrors.ubuntu.com/%s.txt"
	launchpadMirrors = "https://launchpad.net/ubuntu/+archivemirrors"
	userAgent        = "aptmir/1.0 (+https://example.invalid)"
)

// portsArches are the architectures served from ports.ubuntu.com rather than
// the main archive.
var portsArches = map[string]bool{
	"arm64": true, "armhf": true, "ppc64el": true,
	"s390x": true, "riscv64": true, "powerpc": true,
}

type config struct {
	codename        string
	arch            string
	country         string
	concurrency     int
	timeout         time.Duration
	probeBytes      int64
	probeTime       time.Duration
	maxAge          time.Duration
	limit           int
	probeTop        int
	screenTop       int
	noCache         bool
	includeCSP      bool
	showVersion     bool
	scheme          string
	skipBandwidth   bool
	jsonOut         bool
	apply           bool
	dryRun          bool
	includeSecurity bool
	debug           bool
}

// Mirror holds everything measured about a single candidate.
type Mirror struct {
	URL           string        `json:"url"`
	Country       string        `json:"country,omitempty"`
	Reachable     bool          `json:"reachable"`
	Updating      bool          `json:"update_in_progress"`
	Response      time.Duration `json:"response_ns"`
	CDN           string        `json:"cdn,omitempty"`
	Pool          bool          `json:"pool"`
	Released      time.Time     `json:"release_date,omitzero"`
	Behind        time.Duration `json:"behind_ns"`
	Bandwidth     float64       `json:"bandwidth_bytes_per_sec"`
	Err           string        `json:"error,omitempty"`
	MarkerStatus  string        `json:"sync_marker,omitempty"`
	Verified      bool          `json:"indexes_verified"`
	Demoted       bool          `json:"demoted"`
	LowConfidence bool          `json:"bandwidth_low_confidence"`
}

// usable reports whether a mirror is a legitimate candidate: it answered
// without error, published a release date, and is not staler than the
// caller's tolerance. A mirror with a verified sync lock is still usable:
// screen sets Demoted on it, and rank and measureTop tier on that flag rather
// than excluding it here. -apply is stricter still; see selectForApply.
func (m *Mirror) usable(maxAge time.Duration) bool {
	if !m.Reachable || m.Err != "" {
		return false
	}
	if m.Released.IsZero() {
		return false
	}
	return m.Behind <= maxAge
}

func main() {
	cfg := parseFlags()

	// The version is a result, not a status line, so it goes to stdout and
	// nothing else is printed alongside it.
	if cfg.showVersion {
		fmt.Println(versionString())
		return
	}
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// registerFlags binds every flag onto fs.
//
// It is separate from parseFlags so the bindings can be exercised against a
// throwaway FlagSet rather than os.Args. -debug once went a whole release
// unbound, accepted by nobody and reported by nothing, which is the failure
// this separation exists to catch.
func registerFlags(fs *flag.FlagSet, cfg *config) {
	fs.StringVar(&cfg.codename, "codename", "", "release codename (default: autodetect)")
	fs.StringVar(&cfg.arch, "arch", "", "dpkg architecture (default: autodetect)")
	fs.StringVar(&cfg.country, "country", "",
		"restrict to these countries, comma-separated two-letter codes, e.g. DE,PL")
	fs.IntVar(&cfg.concurrency, "concurrency", 30, "parallel probes")
	fs.DurationVar(&cfg.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.Int64Var(&cfg.probeBytes, "probe-bytes", measureBytes,
		"bytes timed per bandwidth probe, measured after a fixed warm-up is discarded")
	fs.DurationVar(&cfg.probeTime, "probe-time", 4*time.Second, "max duration per bandwidth probe")
	fs.DurationVar(&cfg.maxAge, "max-age", 24*time.Hour, "reject mirrors staler than this")
	fs.IntVar(&cfg.limit, "limit", 0,
		"cap the candidate list at this many mirrors (0 = no cap)")
	fs.IntVar(&cfg.probeTop, "probe-top", 10,
		"measure bandwidth on this many best candidates (0 = all)")
	fs.IntVar(&cfg.screenTop, "screen-top", 50,
		"check freshness and sync locks on this many closest candidates (0 = all)")
	fs.BoolVar(&cfg.includeCSP, "include-csp", false,
		"also consider the cloud provider archives (AWS regional, Azure), which are "+
			"listed neither on Launchpad nor in the geo lists")
	fs.BoolVar(&cfg.noCache, "no-cache", false,
		"force every probe past any CDN cache, measuring the cold path a first "+
			"fetch pays rather than the warm path repeated use sees")
	fs.StringVar(&cfg.scheme, "scheme", schemeAny,
		"restrict candidates by URL scheme: http, https, or any")
	fs.BoolVar(&cfg.skipBandwidth, "no-bandwidth", false, "skip throughput probes, rank on latency")
	fs.BoolVar(&cfg.jsonOut, "json", false, "emit JSON instead of a table")
	fs.BoolVar(&cfg.apply, "apply", false, "rewrite apt sources to the winning mirror")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "with -apply, show the diff without writing")
	fs.BoolVar(&cfg.includeSecurity, "include-security", false,
		"also redirect security.ubuntu.com entries (not recommended)")
	fs.BoolVar(&cfg.debug, "d", false, "trace every request, result and measurement on stderr")
	fs.BoolVar(&cfg.debug, "debug", false,
		"trace every request, result and measurement on stderr (same as -d)")
	// -v and -version are the same flag, matching ghget.
	fs.BoolVar(&cfg.showVersion, "version", false, "print version information and exit")
	fs.BoolVar(&cfg.showVersion, "v", false, "print version information and exit (shorthand)")
}

func parseFlags() *config {
	cfg := &config{}
	registerFlags(flag.CommandLine, cfg)
	flag.Parse()
	return cfg
}

// validateConfig rejects flag values that cannot produce a meaningful run.
// Several of them fail in ways that are hard to recognise from the outside:
// -concurrency 0 makes screenAll's semaphore unbuffered and the whole phase
// deadlocks, -concurrency -1 panics inside make, and -probe-bytes 0 quietly
// degrades every bandwidth probe to the whole-transfer fallback. Refusing them
// up front is the difference between a clear message and a hang.
func validateConfig(cfg *config) error {
	positive := []struct {
		flag  string
		value int64
	}{
		{"-concurrency", int64(cfg.concurrency)},
		{"-probe-bytes", cfg.probeBytes},
		{"-probe-time", int64(cfg.probeTime)},
		{"-timeout", int64(cfg.timeout)},
	}
	for _, p := range positive {
		if p.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", p.flag)
		}
	}
	nonNegative := []struct {
		flag  string
		value int
	}{
		{"-probe-top", cfg.probeTop},
		{"-screen-top", cfg.screenTop},
		{"-limit", cfg.limit},
	}
	for _, n := range nonNegative {
		if n.value < 0 {
			return fmt.Errorf("%s must not be negative (0 means no limit)", n.flag)
		}
	}
	if _, err := parseCountries(cfg.country); err != nil {
		return fmt.Errorf("-country: %w", err)
	}
	// An empty scheme is the zero config, which schemeAllowed already treats as
	// no filtering; only a value the user actually typed can be wrong.
	switch cfg.scheme {
	case "", schemeAny, schemeHTTP, schemeHTTPS:
	default:
		return fmt.Errorf("-scheme must be one of %s, %s or %s", schemeHTTP, schemeHTTPS, schemeAny)
	}
	return nil
}

func run(ctx context.Context, cfg *config) error {
	if cfg.debug {
		enableDebug(os.Stderr)
	}

	// Name the program once, then let every status line stand without a
	// prefix. The blank line separates the banner from the status block.
	announce(versionString(), "aptmir", "version", version, "commit", commit, "built", built)
	blankLine()

	if cfg.arch == "" {
		cfg.arch = detectArch(ctx)
	}
	if cfg.codename == "" {
		cn, err := detectCodename(ctx)
		if err != nil {
			return err
		}
		cfg.codename = cn
	}
	debugLog.Debug("resolved", "codename", cfg.codename, "arch", cfg.arch)

	client := newClient(cfg.timeout)

	eol, err := releaseIsEOL(ctx, client, cfg)
	if err != nil {
		debugLog.Debug("eol check failed", "err", shortErr(err))
	}
	if eol {
		fmt.Fprintf(os.Stderr,
			"%s appears to be end-of-life; mirrors no longer carry it.\n"+
				"         Point your sources at %s instead.\n", cfg.codename, oldReleases)
		return nil
	}

	announce(fmt.Sprintf("discovering mirrors for %s/%s...", cfg.codename, cfg.arch),
		"discovering mirrors", "codename", cfg.codename, "arch", cfg.arch)
	candidates, knownArchives := discover(ctx, client, cfg)
	if len(candidates) == 0 {
		return errors.New("no candidate mirrors found")
	}
	if cfg.limit > 0 && len(candidates) > cfg.limit {
		candidates = candidates[:cfg.limit]
	}
	debugLog.Debug("discovered", "candidates", len(candidates), "arch", cfg.arch, "scheme", cfg.scheme)
	// Phase 1a: a response-time sweep over every candidate. It is the only
	// affordable way to tell 800 mirrors apart, and it decides which few are
	// worth spending a 130 KB InRelease fetch on.
	announce(fmt.Sprintf("measuring response time to %d candidates...", len(candidates)),
		"response sweep", "candidates", len(candidates))
	sweepLatency(ctx, client, cfg, candidates)
	candidates = nearest(candidates, cfg.screenTop)

	announce(fmt.Sprintf("screening %d closest candidates (freshness, sync locks)...", len(candidates)),
		"screening", "candidates", len(candidates))

	reference := referenceDate(ctx, client, cfg)
	mirrors := screenAll(ctx, client, cfg, candidates)

	// Behind has to be known before phase 2, not after it: measureTop has only
	// -probe-top slots, and staleness is what decides whether a mirror is worth
	// spending one on. Computed afterwards, a handful of stale-but-nearby
	// mirrors can take every slot and leave the run without a bandwidth figure
	// for any mirror it would actually recommend.
	applyBehind(mirrors, resolveReference(reference, mirrors))

	if !cfg.skipBandwidth {
		announce(fmt.Sprintf("measuring bandwidth on %s, one at a time...", measureScope(cfg)),
			"bandwidth sweep", "scope", measureScope(cfg))
		mirrors = measureTop(ctx, client, cfg, mirrors)
	}

	ranked := rank(mirrors, cfg)

	// Close the status block so the results start clean. It belongs on stderr
	// with the rest of the status, leaving stdout as nothing but results.
	blankLine()

	if cfg.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ranked)
	}
	printTable(os.Stdout, ranked)
	printSummary(os.Stdout, ranked)

	if !cfg.apply {
		return nil
	}
	best, fellBack := selectForApply(ranked, cfg)
	if best == nil {
		return errors.New("no mirror met the freshness and availability criteria for -apply; " +
			"mirrors that are mid-sync are never written to /etc/apt, even as a fallback. " +
			"Nothing was changed")
	}
	if fellBack {
		fmt.Fprintf(os.Stderr,
			"no clean mirror qualified; falling back to %s, which carries a leaked\n"+
				"         sync lock (older than an hour, or of unreadable age) whose tree passed\n"+
				"         index verification against its own release file.\n", best.URL)
	}
	return applyMirror(cfg, best.URL, knownArchives)
}

// selectForApply picks the mirror to write into apt sources. Rewriting
// sources is persistent, so a mirror whose sync lock leaked is used only
// when no clean mirror qualifies, and the caller is told when that happened.
func selectForApply(ranked []*Mirror, cfg *config) (*Mirror, bool) {
	// A mirror whose lock is fresh is mid-rsync right now. Verification of such
	// a tree is valid only for the instant it ran, because the tree is being
	// rewritten underneath it — unlike a leaked lock, where the tree is static
	// and a passing verification means something an hour later. Ranking and the
	// table still show it, demoted; -apply writes /etc/apt persistently, so it
	// never selects one, not even as a fallback.
	acceptable := func(m *Mirror) bool { return m.usable(cfg.maxAge) && !m.Updating }
	for _, m := range ranked {
		if acceptable(m) && !m.Demoted {
			return m, false
		}
	}
	for _, m := range ranked {
		if acceptable(m) {
			return m, true
		}
	}
	return nil, false
}

// maxClockSkew is how far past local time a release date may sit before it is
// disregarded. InRelease is signed, but aptmir does not verify the signature,
// so without this a mirror serving a forged future date would become the
// yardstick, make every honest mirror look stale, and hand itself the top spot.
const maxClockSkew = time.Hour

// resolveReference returns the point every mirror's staleness is measured
// against: the freshest credible release date available, whether that comes
// from the canonical archive or from the candidates themselves.
//
// It takes the maximum rather than preferring the archive, because
// archive.ubuntu.com is a pool. A backend part-way through a sync answers
// successfully with the previous, older tree, and the earlier version returned
// that date unchallenged — the candidate scan ran only when the fetch failed
// outright. Every mirror ahead of a lagging backend then floored to zero and
// reported "current", so the column quietly stopped discriminating instead of
// showing anything wrong.
func resolveReference(reference time.Time, mirrors []*Mirror) time.Time {
	cutoff := time.Now().Add(maxClockSkew)
	if reference.After(cutoff) {
		reference = time.Time{}
	}
	for _, m := range mirrors {
		// A mirror that failed screening keeps whatever Released it read before
		// failing. Letting it set the yardstick would measure every other
		// mirror's staleness against a tree we just refused to trust. Err is
		// the precise signal: anything that reached a usable release date
		// without an error is fair game, reachable or not.
		if m.Err != "" || m.Released.After(cutoff) {
			continue
		}
		if m.Released.After(reference) {
			reference = m.Released
		}
	}
	return reference
}

// applyBehind records how far each mirror trails the reference point.
func applyBehind(mirrors []*Mirror, reference time.Time) {
	for _, m := range mirrors {
		if m.Released.IsZero() {
			continue
		}
		m.Behind = max(reference.Sub(m.Released), 0)
	}
}

func newClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   2,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr}
}

// warnf reports degraded service. Unlike logf it is not gated on -v, because
// the whole point is that the user learns the run is worth less than it looks.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// announce reports a phase the run has reached.
//
// Under -d it joins the trace as logfmt, so everything above the results is one
// parseable stream rather than two formats interleaved. Otherwise it is the
// plain status line a person reads. The two texts differ because they are for
// different readers: one is a sentence, the other is fields.
func announce(human, event string, kv ...any) {
	if debugging() {
		debugLog.Debug(event, kv...)
		return
	}
	progressf("%s", human)
}

// blankLine separates the status block from the banner and the results. Under
// -d it is skipped, since a bare newline is not logfmt.
func blankLine() {
	if debugging() {
		return
	}
	progressf("")
}

// progressf names the phase a run has reached. It always goes to stderr, so a
// run that spends half a minute probing does not look hung, while stdout stays
// clean for the table and for -json.
func progressf(format string, args ...any) {
	progressTo(os.Stderr, format, args...)
}

// progressTo is progressf with the destination injected, so tests can assert
// that progress never lands on stdout.
func progressTo(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// measureScope describes what phase 2 is about to measure, for the progress
// line. -probe-top 0 means every usable candidate.
func measureScope(cfg *config) string {
	if cfg.probeTop <= 0 {
		return "every usable candidate"
	}
	return fmt.Sprintf("the %d fastest candidates", cfg.probeTop)
}

// debugLog traces the run when -d is set.
//
// It discards until enableDebug points it somewhere, so the call sites need no
// enabled check. slog's TextHandler emits logfmt and is safe for concurrent
// use, which matters because screening and the sweep trace from -concurrency
// goroutines at once.
var debugLog = slog.New(slog.DiscardHandler)

// debugOn mirrors whether the trace is enabled. Asking the handler would mean
// a context, and the callers that need to know — the progress reporter — have
// no business taking one just to answer it.
var debugOn atomic.Bool

// enableDebug sends the trace to w at debug level.
func enableDebug(w io.Writer) {
	debugLog = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	debugOn.Store(true)
}

// disableDebug silences the trace again. Tests restore state with it.
func disableDebug() {
	debugLog = slog.New(slog.DiscardHandler)
	debugOn.Store(false)
}

// debugging reports whether the trace is on, for the few decisions that turn on
// it rather than merely logging.
func debugging() bool {
	return debugOn.Load()
}

// ---------------------------------------------------------------- detection

func detectArch(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "dpkg", "--print-architecture").Output()
	if err == nil {
		if a := strings.TrimSpace(string(out)); a != "" {
			return a
		}
	}
	return "amd64"
}

// detectCodename prefers /etc/os-release, which is authoritative for the
// running system and present on every supported release. The sources files
// are only consulted as a fallback, and then only for stanzas that actually
// point at an Ubuntu archive, so a third-party repo such as NodeSource
// (Suites: nodistro) cannot be mistaken for the release codename.
func detectCodename(ctx context.Context) (string, error) {
	if cn := codenameFromOSRelease(); cn != "" {
		return cn, nil
	}
	if cn := codenameFromDeb822(); cn != "" {
		return cn, nil
	}
	if cn := codenameFromLegacy(); cn != "" {
		return cn, nil
	}
	out, err := exec.CommandContext(ctx, "lsb_release", "-cs").Output()
	if err == nil {
		if cn := strings.TrimSpace(string(out)); cn != "" {
			return cn, nil
		}
	}
	return "", errors.New("could not determine the release codename; pass -codename")
}

// baseSuite strips the -updates/-security/-backports/-proposed qualifier.
func baseSuite(s string) string {
	for _, suffix := range []string{"-updates", "-security", "-backports", "-proposed"} {
		if base, ok := strings.CutSuffix(s, suffix); ok {
			return base
		}
	}
	return s
}

// isUbuntuArchive reports whether a URI belongs to the Ubuntu archive proper
// rather than a PPA or an unrelated third-party repository.
func isUbuntuArchive(uri string) bool {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	if strings.Contains(host, "ppa.launchpad") {
		return false
	}
	return strings.HasSuffix(host, "ubuntu.com") ||
		strings.Contains(strings.ToLower(u.Path), "/ubuntu")
}

// codenameFromDeb822 walks each stanza, keeping URIs and Suites together so
// that a suite is only trusted when its stanza points at an Ubuntu archive.
func codenameFromDeb822() string {
	paths, _ := filepath.Glob("/etc/apt/sources.list.d/*.sources")
	for _, p := range paths {
		//nolint:gosec // G304: p is a filepath.Glob match under /etc/apt, not caller input.
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for stanza := range strings.SplitSeq(string(data), "\n\n") {
			var uris, suites []string
			for line := range strings.SplitSeq(stanza, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				lower := strings.ToLower(line)
				switch {
				case strings.HasPrefix(lower, "uris:"):
					uris = append(uris, strings.Fields(line[len("uris:"):])...)
				case strings.HasPrefix(lower, "suites:"):
					suites = append(suites, strings.Fields(line[len("suites:"):])...)
				}
			}
			if !slices.ContainsFunc(uris, isUbuntuArchive) {
				continue
			}
			for _, s := range suites {
				if b := baseSuite(s); b == s && b != "" {
					return b
				}
			}
			if len(suites) > 0 {
				return baseSuite(suites[0])
			}
		}
	}
	return ""
}

func codenameFromLegacy() string {
	paths := []string{"/etc/apt/sources.list"}
	more, _ := filepath.Glob("/etc/apt/sources.list.d/*.list")
	paths = append(paths, more...)
	for _, p := range paths {
		if cn := codenameFromLegacyFile(p); cn != "" {
			return cn
		}
	}
	return ""
}

// codenameFromLegacyFile scans one one-line-format sources file and returns
// the first unqualified suite belonging to an Ubuntu archive.
func codenameFromLegacyFile(path string) string {
	//nolint:gosec // G304: path is a fixed /etc/apt location or a glob match, not caller input.
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		// deb [opts] URI suite components...
		if len(fields) < 3 || (fields[0] != "deb" && fields[0] != "deb-src") {
			continue
		}
		i := 1
		for i < len(fields) && strings.HasPrefix(fields[i], "[") {
			for i < len(fields) && !strings.HasSuffix(fields[i], "]") {
				i++
			}
			i++
		}
		if i+1 >= len(fields) {
			continue
		}
		if !isUbuntuArchive(fields[i]) {
			continue
		}
		suite := fields[i+1]
		if b := baseSuite(suite); b == suite {
			return b
		}
	}
	// A read error mid-file is not the same as reaching the end: without this
	// a truncated sources file silently reports "no codename found".
	if err := sc.Err(); err != nil {
		return ""
	}
	return ""
}

func codenameFromOSRelease() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if after, ok := strings.CutPrefix(line, "VERSION_CODENAME="); ok {
			return strings.Trim(after, `"`)
		}
	}
	// Distinguish a genuine absence from a failed read, so the caller falls
	// through to the other detectors rather than trusting an empty answer.
	if err := sc.Err(); err != nil {
		return ""
	}
	return ""
}

func (cfg *config) archiveRoot() string {
	if portsArches[cfg.arch] {
		return defaultPorts
	}
	return defaultArchive
}

// ---------------------------------------------------------------- discovery

func discover(ctx context.Context, client *http.Client, cfg *config) ([]*Mirror, map[string]struct{}) {
	seen := map[string]*Mirror{}
	knownArchives := map[string]struct{}{}
	add := func(raw, country string) {
		raw = normalizeMirrorURL(raw)
		if raw == "" {
			return
		}
		// Every input to add came from an Ubuntu-operated mirror catalogue or
		// from the built-in canonical/cloud-provider list. Keep that provenance
		// even when the candidate is later filtered by architecture or scheme,
		// so -apply can safely recognise an already-configured official mirror.
		knownArchives[raw] = struct{}{}
		// One gate for every source: the geo list, the canonical archive root
		// and the Launchpad scrape, whose regexp matches ubuntu-ports too.
		if !servesArch(raw, cfg.arch) || !schemeAllowed(raw, cfg.scheme) {
			return
		}
		if m, ok := seen[raw]; ok {
			if m.Country == "" {
				m.Country = country
			}
			return
		}
		seen[raw] = &Mirror{URL: raw, Country: country}
	}

	var order []string

	if portsArches[cfg.arch] {
		// mirrors.ubuntu.com only indexes the main archive, so ports users
		// start from the canonical host and rely on Launchpad for the rest.
		add(defaultPorts, "")
		order = append(order, normalizeMirrorURL(defaultPorts))
	} else {
		// One list per country, merged in the order the codes were given. The
		// service publishes a separate list per country, so several countries
		// mean several fetches rather than one filtered result.
		countries, err := parseCountries(cfg.country)
		if err != nil {
			debugLog.Debug("country list rejected", "err", err)
		}
		type geoList struct{ url, country string }
		lists := []geoList{{url: geoMirrorList}}
		if len(countries) > 0 {
			lists = lists[:0]
			for _, code := range countries {
				lists = append(lists, geoList{
					url:     fmt.Sprintf(countryMirrorFmt, code),
					country: code,
				})
			}
		}
		for _, l := range lists {
			urls, err := fetchMirrorList(ctx, client, l.url)
			if err != nil {
				debugLog.Debug("mirror list failed", "url", l.url, "err", shortErr(err))
				continue
			}
			for _, u := range urls {
				add(u, l.country)
				order = append(order, normalizeMirrorURL(u))
			}
		}
		add(cfg.archiveRoot(), "")
		order = append(order, normalizeMirrorURL(cfg.archiveRoot()))
	}

	if cfg.includeCSP {
		// Not narrowed by -country: these are named directly rather than
		// discovered, and AWS regions do not map onto country codes. Asking
		// for them is the opt-in.
		for _, u := range cspMirrors(cfg.arch) {
			add(u, "")
			order = append(order, normalizeMirrorURL(u))
		}
	}

	{
		// Launchpad is always consulted. It is the only source naming every
		// scheme a mirror offers, and for a ports architecture it is the only
		// source of alternatives at all. Failure here is degraded service, not
		// a fatal error, but it is loud: a run quietly losing most of its
		// candidates is exactly what nobody notices.
		lp, err := fetchLaunchpadMirrors(ctx, client)
		if err != nil {
			warnf("could not read the Launchpad mirror list (%v);\n"+
				"         continuing with the geo list alone, which lists fewer mirrors\n"+
				"         and only one scheme per mirror.", shortErr(err))
		}
		countries, err := parseCountries(cfg.country)
		if err != nil {
			debugLog.Debug("country list rejected", "err", err)
		}
		want := map[string]bool{}
		for _, code := range countries {
			// Launchpad groups mirrors under 84 country headings; the geo
			// service knows more codes than that. An unmapped code simply
			// means Launchpad contributes nothing for it, which is worth
			// saying but is no longer an error now that it is always used.
			if _, ok := launchpadCountryNames[code]; !ok {
				warnf("Launchpad lists no mirrors for -country %s; using the geo list for it", code)
			}
			want[code] = true
		}
		for _, m := range lp {
			// Launchpad is fetched globally before -country narrows the ranking.
			// Remember every official URL so a source using a mirror in another
			// country can still be replaced without relying on hostname guesses.
			if n := normalizeMirrorURL(m.URL); n != "" {
				knownArchives[n] = struct{}{}
			}
			// -country narrows Launchpad too. Without this the flag is
			// silently ignored whenever -launchpad is given, and a run asking
			// for one country quietly widens to every mirror in the world.
			if len(want) > 0 && !want[m.Country] {
				continue
			}
			n := normalizeMirrorURL(m.URL)
			if _, ok := seen[n]; !ok {
				order = append(order, n)
			}
			add(m.URL, m.Country)
		}
	}

	out := make([]*Mirror, 0, len(seen))
	added := map[string]bool{}
	for _, u := range order {
		if m, ok := seen[u]; ok && !added[u] {
			out = append(out, m)
			added[u] = true
		}
	}
	return out, knownArchives
}

// parseCountries splits the -country value into upper-case ISO 3166-1 alpha-2
// codes, preserving order and dropping repeats. An empty entry is rejected
// rather than treated as "everywhere", because "de,,pl" is a typo and silently
// widening the search is the failure this flag exists to prevent.
func parseCountries(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if len(code) != 2 {
			return nil, fmt.Errorf("country %q is not a two-letter code", strings.TrimSpace(part))
		}
		for _, r := range code {
			if r < 'A' || r > 'Z' {
				return nil, fmt.Errorf("country %q is not a two-letter code", strings.TrimSpace(part))
			}
		}
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	return out, nil
}

// cachingAllowed reports whether probes may be served from a CDN cache.
//
// Caching is allowed by default because it is closer to what apt actually
// does: it fetches hundreds of files over reused connections, so the cold
// first-fetch penalty is amortised rather than paid per file, and on a fleet
// the edge is warm for everyone after the first machine. -no-cache forces the
// cold path, which is the honest figure for a single one-off fetch.
func (cfg *config) cachingAllowed() bool { return !cfg.noCache }

// Cloud providers run archive mirrors for their own instances. They appear
// neither on Launchpad nor in the geo lists, so they are only reachable by
// naming them, which -include-csp does.
//
// Only these two are offered. Google's gce.archive.ubuntu.com does not resolve
// publicly at all, and Oracle's oci.archive.ubuntu.com resolves to Canonical's
// own addresses from outside, so it is not a distinct mirror for anyone who
// could reach it here.
//
// Azure is a single host. AWS has no unprefixed name, so the region is
// mandatory, and nothing tells us which region a machine is in: every region is
// offered and the response-time sweep picks the nearest. All 28 below resolved
// and served the archive when checked.
const azureMirror = "http://azure.archive.ubuntu.com/ubuntu/"

// ec2MirrorFmt builds a regional AWS archive URL from a scheme and region.
const ec2MirrorFmt = "%s://%s.ec2.archive.ubuntu.com/ubuntu/"

var ec2Regions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"ca-central-1", "ca-west-1", "sa-east-1",
	"eu-west-1", "eu-west-2", "eu-west-3",
	"eu-central-1", "eu-central-2", "eu-north-1", "eu-south-1", "eu-south-2",
	"ap-south-1", "ap-south-2",
	"ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
	"me-south-1", "me-central-1", "af-south-1", "il-central-1",
}

// isPoolHost reports whether a URL's host is one of Canonical's rotations
// rather than a single server.
//
// These names answer from many addresses — archive.ubuntu.com from 9,
// security.ubuntu.com and pl.archive.ubuntu.com from 9, us-east-1.ec2 from 10 —
// while a real mirror answers from one. azure.archive.ubuntu.com is a rotation
// by a different mechanism, resolving through Azure Traffic Manager.
//
// It matters because the backends are not always in the same state: the
// Archive-Update-in-Progress marker appeared on 2 of 10 requests to
// archive.ubuntu.com, and its status flapped between runs as a result. A
// measurement of such a host belongs to whichever backend answered rather than
// to a server anyone could pin in their sources, so the table says so instead
// of presenting it as an ordinary mirror.
//
// Matching is on the host alone: ftp.hosteurope.de serves the archive from a
// path containing archive.ubuntu.com and is a single server.
func isPoolHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com":
		return true
	}
	return strings.HasSuffix(host, ".archive.ubuntu.com")
}

// cspMirrors returns the cloud-provider archives worth offering for an
// architecture.
//
// Neither provider publishes a ports tree — both answer 404 for /ubuntu-ports/
// — so a ports run gets nothing rather than 57 candidates that cannot serve it.
//
// Azure is offered over http only: the name has no working TLS, so an https
// entry would be a permanently unreachable row. The regional AWS mirrors serve
// both schemes, so both are offered and -scheme can choose between them.
func cspMirrors(arch string) []string {
	if portsArches[arch] {
		return nil
	}
	out := make([]string, 0, 1+2*len(ec2Regions))
	out = append(out, azureMirror)
	for _, region := range ec2Regions {
		out = append(out,
			fmt.Sprintf(ec2MirrorFmt, schemeHTTP, region),
			fmt.Sprintf(ec2MirrorFmt, schemeHTTPS, region),
		)
	}
	return out
}

// Build metadata, injected at link time by goreleaser as main.version and
// friends. A build made outside the release process reports dev.
var (
	version = "dev"
	commit  = "unknown"
	built   = "unknown"
)

// versionString renders the build metadata for -version and for the banner.
func versionString() string {
	return "aptmir " + version + " (commit " + commit + ", built " + built + ")"
}

// Accepted values for -scheme.
const (
	schemeAny   = "any"
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// isPortsURL reports whether a URL points at the ports archive rather than the
// main one. A mirror publishes the two under different paths, so the URL alone
// decides which architectures it can answer for.
func isPortsURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, "ports.ubuntu.com") ||
		strings.Contains(strings.ToLower(u.Path), "ubuntu-ports")
}

// servesArch reports whether a mirror can serve the given architecture.
// mirrors.ubuntu.com lists ports mirrors alongside main-archive ones, and a
// ports tree carries no binary-amd64 at all: left unfiltered, such a candidate
// spends one of the scarce -probe-top measurement slots only to fail.
func servesArch(raw, arch string) bool {
	return isPortsURL(raw) == portsArches[arch]
}

// schemeAllowed applies the -scheme filter. An empty want means no filtering,
// so a zero config keeps every candidate.
func schemeAllowed(raw, want string) bool {
	if want == "" || want == schemeAny {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, want)
}

// normalizeMirrorURL canonicalises to an http(s) URL with a trailing slash.
func normalizeMirrorURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

func fetchMirrorList(ctx context.Context, client *http.Client, listURL string) ([]string, error) {
	body, err := get(ctx, client, listURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	var out []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// launchpadCountryNames maps ISO 3166-1 alpha-2 codes to the country headings
// Launchpad prints in its mirror listing. Launchpad uses ISO 3166-1 English
// short names, inversions included, so the table is transcribed from the live
// page rather than guessed: "Korea, Republic of", not "South Korea".
//
// Only countries that actually host a mirror are listed. A code that is absent
// is reported to the user rather than silently widening -launchpad to every
// mirror in the world.
var launchpadCountryNames = map[string]string{
	"AE": "United Arab Emirates",
	"AM": "Armenia",
	"AR": "Argentina",
	"AT": "Austria",
	"AU": "Australia",
	"AZ": "Azerbaijan",
	"BA": "Bosnia and Herzegovina",
	"BD": "Bangladesh",
	"BE": "Belgium",
	"BG": "Bulgaria",
	"BR": "Brazil",
	"BW": "Botswana",
	"BY": "Belarus",
	"CA": "Canada",
	"CH": "Switzerland",
	"CL": "Chile",
	"CN": "China",
	"CO": "Colombia",
	"CR": "Costa Rica",
	"CZ": "Czech Republic",
	"DE": "Germany",
	"DK": "Denmark",
	"EC": "Ecuador",
	"EE": "Estonia",
	"ES": "Spain",
	"FI": "Finland",
	"FR": "France",
	"GB": "United Kingdom",
	"GE": "Georgia",
	"GL": "Greenland",
	"GR": "Greece",
	"HK": "Hong Kong",
	"HR": "Croatia",
	"HU": "Hungary",
	"ID": "Indonesia",
	"IE": "Ireland",
	"IL": "Israel",
	"IN": "India",
	"IQ": "Iraq",
	"IR": "Iran, Islamic Republic of",
	"IS": "Iceland",
	"IT": "Italy",
	"JP": "Japan",
	"KE": "Kenya",
	"KG": "Kyrgyzstan",
	"KH": "Cambodia",
	"KR": "Korea, Republic of",
	"KZ": "Kazakhstan",
	"LT": "Lithuania",
	"LU": "Luxembourg",
	"LV": "Latvia",
	"MA": "Morocco",
	"MD": "Moldova, Republic of",
	"MG": "Madagascar",
	"MK": "Macedonia, Republic of",
	"MN": "Mongolia",
	"MY": "Malaysia",
	"NC": "New Caledonia",
	"NL": "Netherlands",
	"NO": "Norway",
	"NZ": "New Zealand",
	"PH": "Philippines",
	"PL": "Poland",
	"PR": "Puerto Rico",
	"PT": "Portugal",
	"RO": "Romania",
	"RS": "Serbia",
	"RU": "Russian Federation",
	"SA": "Saudi Arabia",
	"SE": "Sweden",
	"SG": "Singapore",
	"SI": "Slovenia",
	"SK": "Slovakia",
	"TH": "Thailand",
	"TN": "Tunisia",
	"TR": "Turkey",
	"TW": "Taiwan",
	"TZ": "Tanzania, United Republic of",
	"UA": "Ukraine",
	"UG": "Uganda",
	"US": "United States",
	"UY": "Uruguay",
	"VN": "Viet Nam",
	"ZA": "South Africa",
}

// launchpadCountryCodes is launchpadCountryNames inverted, for tagging mirrors
// as the listing is scanned.
var launchpadCountryCodes = func() map[string]string {
	out := make(map[string]string, len(launchpadCountryNames))
	for code, name := range launchpadCountryNames {
		out[name] = code
	}
	return out
}()

// lpHrefRe pulls link targets out of the Launchpad listing. The page is read
// for links rather than scanned for archive URLs directly, because a mirror
// publishes its archive at whatever depth it likes.
var lpHrefRe = regexp.MustCompile(`href="([a-zA-Z][a-zA-Z0-9+.\-]*://[^"]+)"`)

// isArchiveURL reports whether a link from the Launchpad mirror listing points
// at an Ubuntu package archive apt could actually fetch from.
//
// rsync and ftp URLs are rejected on purpose rather than overlooked: rsync is a
// mirror-administration protocol apt cannot speak at all, and apt removed its
// ftp method in 2.0. Launchpad lists all three for most mirrors.
func isArchiveURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Host)
	// Launchpad links its own pages and every PPA from the same listing.
	if strings.Contains(host, "launchpad") || strings.HasPrefix(host, "ppa.") {
		return false
	}
	// Any mention of ubuntu in the path, at any depth and in any case. A
	// stricter whitelist of directory names cannot keep up with how mirrors
	// actually name things: artfiles.org serves /ubuntu.com/, bacloud serves
	// /ubuntu-mirror/archive/ and lyrahosting serves /ubuntuarchive/. Being
	// permissive here is the safe direction — a candidate that is not really
	// an archive fails screening and is reported unreachable, whereas a mirror
	// that is never extracted is invisible.
	return strings.Contains(strings.ToLower(u.Path), "ubuntu")
}

// lpCountryRe matches the heading that introduces each country's group of
// mirrors in the listing table.
var lpCountryRe = regexp.MustCompile(`<th[^>]*colspan="2"[^>]*>([^<]*)</th>`)

// lpMirror is one archive URL from the Launchpad listing, tagged with the
// country whose heading it appeared under.
type lpMirror struct {
	URL     string
	Country string
}

// parseLaunchpadMirrors extracts every usable archive URL from the listing page,
// preserving order and keeping each scheme a mirror offers, because -scheme
// chooses between them and never rewrites a URL. Each is tagged with the
// country it is grouped under, so -country can narrow Launchpad the same way
// it narrows the geo list.
func parseLaunchpadMirrors(page []byte) []lpMirror {
	type heading struct {
		at   int
		code string
	}
	var headings []heading
	for _, m := range lpCountryRe.FindAllSubmatchIndex(page, -1) {
		name := strings.TrimSpace(string(page[m[2]:m[3]]))
		headings = append(headings, heading{at: m[0], code: launchpadCountryCodes[name]})
	}
	// The listing is a flat table, so a mirror belongs to the last country
	// heading that appears before it.
	countryAt := func(pos int) string {
		code := ""
		for _, h := range headings {
			if h.at > pos {
				break
			}
			code = h.code
		}
		return code
	}

	seen := map[string]bool{}
	var out []lpMirror
	for _, m := range lpHrefRe.FindAllSubmatchIndex(page, -1) {
		raw := string(page[m[2]:m[3]])
		if seen[raw] || !isArchiveURL(raw) {
			continue
		}
		seen[raw] = true
		out = append(out, lpMirror{URL: raw, Country: countryAt(m[0])})
	}
	return out
}

func fetchLaunchpadMirrors(ctx context.Context, client *http.Client) ([]lpMirror, error) {
	body, err := get(ctx, client, launchpadMirrors)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseLaunchpadMirrors(data), nil
}

// ---------------------------------------------------------------- EOL check

func releaseIsEOL(ctx context.Context, client *http.Client, cfg *config) (bool, error) {
	live := cfg.archiveRoot() + "dists/" + cfg.codename + "/Release"
	ok, err := headOK(ctx, client, live)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil
	}
	old := oldReleases + "dists/" + cfg.codename + "/Release"
	return headOK(ctx, client, old)
}

func referenceDate(ctx context.Context, client *http.Client, cfg *config) time.Time {
	t, err := fetchReleaseDate(ctx, client, cfg, cfg.archiveRoot())
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---------------------------------------------------------------- ranking

func rank(mirrors []*Mirror, cfg *config) []*Mirror {
	out := make([]*Mirror, len(mirrors))
	copy(out, mirrors)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		au, bu := a.usable(cfg.maxAge), b.usable(cfg.maxAge)
		if au != bu {
			return au
		}
		if !au {
			return false
		}
		if a.Demoted != b.Demoted {
			return !a.Demoted
		}
		if !cfg.skipBandwidth && a.Bandwidth != b.Bandwidth {
			return a.Bandwidth > b.Bandwidth
		}
		if a.Response != b.Response {
			if a.Response == 0 {
				return false
			}
			if b.Response == 0 {
				return true
			}
			return a.Response < b.Response
		}
		return a.Behind < b.Behind
	})
	return out
}

// ---------------------------------------------------------------- output

func printTable(w io.Writer, mirrors []*Mirror) {
	_, _ = fmt.Fprintf(w, "%-4s %-52s %10s %9s %10s  %s\n",
		"#", "MIRROR", "BANDWIDTH", "RESPONSE", "BEHIND", "STATUS")
	for i, m := range mirrors {
		status := "ok"
		switch {
		case m.Err != "":
			status = "unreachable: " + truncate(m.Err, maxStatusErr)
		case m.MarkerStatus != "":
			status = m.MarkerStatus
		}
		// Low confidence is signalled in STATUS rather than appended to the
		// bandwidth cell: BANDWIDTH has a fixed width so every row lines up,
		// and appending here would push it past that width only on the rows
		// this feature exists to flag, misaligning exactly those rows.
		// A caching front end is worth naming rather than hiding: it inverts
		// which mirror is best. Cold, such a mirror served 5.6 MB/s; warm off
		// the edge it served 43-51 MB/s, beating a direct regional mirror. One
		// machine pays the cold path, a fleet mostly gets the warm one.
		// A rotation is not a server: the figures belong to whichever backend
		// answered and are not reproducible, so say so rather than letting it
		// sit in the table looking like an ordinary mirror.
		if m.Pool {
			status += " (pool)"
		}
		if m.CDN != "" {
			status += " (via " + m.CDN + ")"
		}
		if m.LowConfidence {
			status += " (low-confidence)"
		}
		_, _ = fmt.Fprintf(w, "%-4d %-52s %10s %9s %10s  %s\n",
			i+1,
			truncate(m.URL, 52),
			formatRate(m.Bandwidth),
			formatDuration(m.Response),
			formatBehind(m),
			status,
		)
	}
}

// maxStatusErr is how much of a mirror's error the STATUS column will show.
// STATUS is the last column, so a long value costs nothing in alignment, and
// the most diagnostic part of the longest error there is — "sha256 <64 hex>
// does not match release file <64 hex>" — sits at the very end of a roughly
// 220-character string. Cutting at 40 threw away exactly the part worth
// reading.
const maxStatusErr = 240

// sampleStats summarises one measured column of the table.
type sampleStats struct {
	n                             int
	min, p50, mean, p90, p95, max float64
}

// percentile returns the nearest-rank percentile of an already sorted sample:
// the smallest value at or above which p percent of the sample falls. Nearest
// rank rather than interpolation, because these samples are small and an
// interpolated figure would imply a precision they do not carry.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// summarize describes a sample, reporting false when there is nothing to
// describe.
func summarize(values []float64) (sampleStats, bool) {
	if len(values) == 0 {
		return sampleStats{}, false
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return sampleStats{
		n:    len(sorted),
		min:  sorted[0],
		p50:  percentile(sorted, 50),
		mean: sum / float64(len(sorted)),
		p90:  percentile(sorted, 90),
		p95:  percentile(sorted, 95),
		max:  sorted[len(sorted)-1],
	}, true
}

// printSummary writes the per-column summary below the table, separated from it
// by a blank line.
//
// The two rows carry their own sample counts because they are not drawn from
// the same set: every screened mirror has a response time, while only the
// -probe-top fastest are measured for bandwidth. With five bandwidth samples a
// p90 is barely distinguishable from the maximum, and SAMPLES is what makes
// that visible rather than implied.
//
// Note that the columns read in opposite directions: for RESPONSE lower is
// better, for BANDWIDTH higher is.
func printSummary(w io.Writer, mirrors []*Mirror) {
	var response, bandwidth []float64
	for _, m := range mirrors {
		if m.Response > 0 {
			response = append(response, float64(m.Response))
		}
		if m.Bandwidth > 0 {
			bandwidth = append(bandwidth, m.Bandwidth)
		}
	}
	rs, haveResponse := summarize(response)
	bs, haveBandwidth := summarize(bandwidth)
	if !haveResponse && !haveBandwidth {
		return
	}

	const row = "%-10s %10s %10s %10s %10s %10s %10s  %s\n"
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, row, "", "MIN", "P50", "MEAN", "P90", "P95", "MAX", "SAMPLES")
	if haveResponse {
		d := func(v float64) string { return formatDuration(time.Duration(v)) }
		_, _ = fmt.Fprintf(w, row, "RESPONSE",
			d(rs.min), d(rs.p50), d(rs.mean), d(rs.p90), d(rs.p95), d(rs.max),
			strconv.Itoa(rs.n))
	}
	if haveBandwidth {
		_, _ = fmt.Fprintf(w, row, "BANDWIDTH",
			formatRate(bs.min), formatRate(bs.p50), formatRate(bs.mean),
			formatRate(bs.p90), formatRate(bs.p95), formatRate(bs.max),
			strconv.Itoa(bs.n))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func formatRate(b float64) string {
	if b <= 0 {
		return "-"
	}
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f kB/s", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", b)
	}
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func formatBehind(m *Mirror) string {
	if m.Released.IsZero() {
		return "-"
	}
	h := m.Behind.Hours()
	if h < 1 {
		return "current"
	}
	return fmt.Sprintf("%.0f h", h)
}

// ---------------------------------------------------------------- apply

func applyMirror(cfg *config, mirror string, knownArchives map[string]struct{}) error {
	files, err := sourceFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("found no apt source files to rewrite")
	}
	changed := 0
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		//nolint:gosec // G304: path comes from sourceFiles, a fixed set of /etc/apt globs.
		orig, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		updated := rewriteSources(string(orig), mirror, cfg, knownArchives)
		if updated == string(orig) {
			continue
		}
		changed++
		if cfg.dryRun {
			fmt.Printf("\n--- %s (dry run)\n%s", path, updated)
			continue
		}
		// The backup and the rewritten file keep whatever mode apt already
		// had on the original; sources files are world-readable by convention.
		mode := info.Mode().Perm()
		backup := fmt.Sprintf("%s.aptmir-%s", path, time.Now().Format("20060102-150405"))
		//nolint:gosec // G306: the backup keeps the original's own mode, whatever apt set.
		if err := os.WriteFile(backup, orig, mode); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
		if err := writeFileAtomic(path, []byte(updated), mode); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("updated %s (backup at %s)\n", path, backup)
	}
	if changed == 0 {
		fmt.Println("no changes needed")
		return nil
	}
	if !cfg.dryRun {
		fmt.Println("run 'sudo apt update' to pick up the new mirror")
	}
	return nil
}

// writeFileAtomic writes data to a temporary file in the target's own directory
// and renames it over the target. This is the only destructive thing the tool
// does and it targets /etc/apt: an in-place write that fails partway leaves a
// truncated sources file, and a multi-file rewrite that fails on file three
// leaves the machine with a mixed set. A rename is atomic on POSIX, so each
// file is either its old contents or its new ones. The temporary name cannot
// match apt's *.list or *.sources globs, so even a crash between the write and
// the rename leaves nothing apt will read.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".aptmir-tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Removes the temporary file on every failure path; after a successful
	// rename the name no longer exists and this is a no-op.
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; sources files are world-readable by
	// convention, so restore whatever mode the original carried.
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sourceFiles() ([]string, error) {
	var out []string
	if _, err := os.Stat("/etc/apt/sources.list"); err == nil {
		out = append(out, "/etc/apt/sources.list")
	}
	for _, pattern := range []string{
		"/etc/apt/sources.list.d/*.list",
		"/etc/apt/sources.list.d/*.sources",
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// sourceURLRe finds URL tokens on deb and deb822 source lines. Whether a token
// belongs to Ubuntu is decided from its parsed hostname or exact catalogue URL;
// the regexp deliberately makes no trust decision of its own.
var sourceURLRe = regexp.MustCompile(`https?://[^\s#]+`)

// isCanonicalArchiveHost recognises Ubuntu-operated archive hostnames. Domain
// boundaries are explicit: archive.ubuntu.com.example.org must not be treated
// as an Ubuntu host merely because its name contains archive.ubuntu.com.
func isCanonicalArchiveHost(host string) bool {
	host = strings.ToLower(host)
	return host == "archive.ubuntu.com" ||
		host == "ports.ubuntu.com" ||
		host == securityHost ||
		host == "ubuntu.osuosl.org" ||
		strings.HasSuffix(host, ".archive.ubuntu.com")
}

// shouldRewriteArchive reports whether raw is an Ubuntu archive root known
// either by its canonical hostname or by exact membership in the official
// mirror catalogues fetched during discovery.
func shouldRewriteArchive(raw string, cfg *config, knownArchives map[string]struct{}) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == securityHost && !cfg.includeSecurity {
		return false
	}
	if isCanonicalArchiveHost(host) {
		return true
	}
	normalized := normalizeMirrorURL(raw)
	_, ok := knownArchives[normalized]
	return ok
}

func rewriteSources(content, mirror string, cfg *config, knownArchives map[string]struct{}) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		isURI := strings.HasPrefix(lower, "uris:")
		isDeb := strings.HasPrefix(lower, "deb ") || strings.HasPrefix(lower, "deb-src ")
		if !isURI && !isDeb {
			continue
		}
		lines[i] = sourceURLRe.ReplaceAllStringFunc(line, func(match string) string {
			if !shouldRewriteArchive(match, cfg, knownArchives) {
				return match
			}
			return strings.TrimSuffix(mirror, "/")
		})
	}
	return strings.Join(lines, "\n")
}
