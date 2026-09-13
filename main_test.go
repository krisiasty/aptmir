package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBaseSuite(t *testing.T) {
	cases := map[string]string{
		"noble":            "noble",
		"noble-updates":    "noble",
		"noble-security":   "noble",
		"noble-backports":  "noble",
		"resolute":         "resolute",
		"resolute-updates": "resolute",
	}
	for in, want := range cases {
		if got := baseSuite(in); got != want {
			t.Errorf("baseSuite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseReleaseTime(t *testing.T) {
	for _, s := range []string{
		"Thu, 11 Sep 2026 14:35:21 UTC",
		"Thu, 11 Sep 2026 14:35:21 +0000",
	} {
		got, err := parseReleaseTime(s)
		if err != nil {
			t.Fatalf("parseReleaseTime(%q): %v", s, err)
		}
		if got.Year() != 2026 || got.Day() != 11 {
			t.Errorf("parseReleaseTime(%q) = %v", s, got)
		}
	}
}

func TestIsUbuntuArchive(t *testing.T) {
	yes := []string{
		"http://archive.ubuntu.com/ubuntu/",
		"http://ports.ubuntu.com/ubuntu-ports/",
		"http://ftp.uni-stuttgart.de/ubuntu/",
	}
	no := []string{
		"https://deb.nodesource.com/node_22.x",
		"http://ppa.launchpadcontent.net/git-core/ppa/ubuntu",
	}
	for _, u := range yes {
		if !isUbuntuArchive(u) {
			t.Errorf("isUbuntuArchive(%q) = false, want true", u)
		}
	}
	for _, u := range no {
		if isUbuntuArchive(u) {
			t.Errorf("isUbuntuArchive(%q) = true, want false", u)
		}
	}
}

func TestNormalizeMirrorURL(t *testing.T) {
	if got := normalizeMirrorURL("http://example.org/ubuntu"); got != "http://example.org/ubuntu/" {
		t.Errorf("missing trailing slash: %q", got)
	}
	if got := normalizeMirrorURL("rsync://example.org/ubuntu"); got != "" {
		t.Errorf("rsync should be rejected: %q", got)
	}
}

func TestRankPutsCleanMirrorsAboveDemoted(t *testing.T) {
	cfg := &config{maxAge: 24 * time.Hour}
	now := time.Now()
	demotedFast := &Mirror{URL: "http://d/", Reachable: true, Released: now, Bandwidth: 90e6, Demoted: true}
	cleanSlow := &Mirror{URL: "http://c/", Reachable: true, Released: now, Bandwidth: 10e6}
	got := rank([]*Mirror{demotedFast, cleanSlow}, cfg)
	if got[0].URL != "http://c/" {
		t.Errorf("ranked first = %s, want the clean mirror even though it is slower", got[0].URL)
	}
}

// tableRows renders mirrors through printTable and splits the result into
// its header line and one line per mirror, trimming the trailing newline.
func tableRows(t *testing.T, mirrors []*Mirror) (header string, rows []string) {
	t.Helper()
	var buf bytes.Buffer
	printTable(&buf, mirrors)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(mirrors)+1 {
		t.Fatalf("got %d lines, want %d (1 header + %d rows): %q", len(lines), len(mirrors)+1, len(mirrors), lines)
	}
	return lines[0], lines[1:]
}

func TestPrintTableErrTakesPrecedenceOverMarkerStatus(t *testing.T) {
	m := &Mirror{URL: "http://e/", Err: "boom", MarkerStatus: "stale-lock"}
	_, rows := tableRows(t, []*Mirror{m})
	row := rows[0]
	if !strings.Contains(row, "unreachable: boom") {
		t.Errorf("row = %q, want it to report the error", row)
	}
	if strings.Contains(row, "stale-lock") {
		t.Errorf("row = %q, want the error to take precedence over the marker status", row)
	}
}

func TestPrintTableStaleLockDistinctFromOK(t *testing.T) {
	now := time.Now()
	clean := &Mirror{URL: "http://c/", Reachable: true, Released: now}
	demoted := &Mirror{
		URL: "http://d/", Reachable: true, Released: now,
		MarkerStatus: "stale-lock", Verified: true, Demoted: true,
	}
	_, rows := tableRows(t, []*Mirror{clean, demoted})
	if !strings.HasSuffix(rows[0], "ok") {
		t.Errorf("clean row = %q, want it to end in ok", rows[0])
	}
	if !strings.HasSuffix(rows[1], "stale-lock") {
		t.Errorf("demoted row = %q, want it to end in stale-lock", rows[1])
	}
	if rows[0] == rows[1] {
		t.Errorf("clean and stale-lock rows should be visibly distinct, both rendered %q", rows[0])
	}
}

// TestPrintTableLowConfidenceStaysAligned reproduces the alignment bug fixed
// in review: a long low-confidence marker on one row must not shift that
// row's LATENCY/BEHIND/STATUS columns relative to every other row. It checks
// this by locating each column's start offset from the header (rather than
// hardcoding indices, so it tracks the format string) and asserting every
// row — including the long one — starts its LATENCY, BEHIND and STATUS
// fields at exactly that offset.
func TestPrintTableLowConfidenceStaysAligned(t *testing.T) {
	now := time.Now()
	mirrors := []*Mirror{
		{URL: "http://a/", Reachable: true, Released: now, Bandwidth: 50 * (1 << 20), Response: 16 * time.Millisecond},
		{
			URL: "http://b/", Reachable: true, Released: now, Bandwidth: 24 * (1 << 20),
			Response: 13 * time.Millisecond, LowConfidence: true,
		},
		{
			URL: "http://c/", Reachable: true, Released: now, Response: 9 * time.Millisecond,
			MarkerStatus: "stale-lock", Verified: true, Demoted: true,
		},
	}
	header, rows := tableRows(t, mirrors)

	// Column start offsets, derived from printTable's own format string
	// ("%-4s %-52s %10s %9s %10s  %s\n") rather than by searching for a
	// label: BANDWIDTH/LATENCY/BEHIND are right-justified, so a label's
	// text does not start where its column does. These offsets are where
	// each field's fixed-width column begins, independent of its content.
	const (
		numW, mirrorW, bwW, latW, behW = 4, 52, 10, 9, 10
		bwIdx                          = numW + 1 + mirrorW + 1
		latIdx                         = bwIdx + bwW + 1
		behIdx                         = latIdx + latW + 1
		stIdx                          = behIdx + behW + 2
	)
	if header[stIdx:] != "STATUS" {
		t.Fatalf("header %q does not have STATUS starting at computed offset %d", header, stIdx)
	}

	for i, row := range rows {
		if len(row) < stIdx {
			t.Fatalf("row %d shorter than the STATUS column start: %q", i, row)
		}
		if row[latIdx-1] != ' ' {
			t.Errorf("row %d: character before LATENCY column (offset %d) is %q, want a space — bandwidth cell overflowed its column: %q", i, latIdx-1, row[latIdx-1], row)
		}
		if row[behIdx-1] != ' ' {
			t.Errorf("row %d: character before BEHIND column (offset %d) is %q, want a space: %q", i, behIdx-1, row[behIdx-1], row)
		}
	}

	// The low-confidence row (index 1) carries the longest per-row text
	// before STATUS; confirm its LATENCY/BEHIND fields still land at the
	// same offsets as the plain row (index 0) and the stale-lock row
	// (index 2), i.e. the columns line up across the mixed set.
	wantLatency := formatDuration(mirrors[1].Response)
	gotLatency := strings.TrimSpace(rows[1][latIdx:behIdx])
	if gotLatency != wantLatency {
		t.Errorf("low-confidence row LATENCY field = %q, want %q (columns misaligned)", gotLatency, wantLatency)
	}
	if !strings.HasPrefix(rows[1][stIdx:], "ok (low-confidence)") {
		t.Errorf("low-confidence row STATUS field = %q, want it to start with %q", rows[1][stIdx:], "ok (low-confidence)")
	}
}

func TestValidateConfig(t *testing.T) {
	valid := func() *config {
		return &config{
			concurrency: 12, probeBytes: 6 << 20, probeTime: 4 * time.Second,
			timeout: 10 * time.Second, probeTop: 5, limit: 20,
		}
	}
	if err := validateConfig(valid()); err != nil {
		t.Fatalf("validateConfig(defaults) = %v, want nil", err)
	}
	// 0 means "no limit" for both of these, and must stay accepted.
	zeroed := valid()
	zeroed.probeTop, zeroed.limit = 0, 0
	if err := validateConfig(zeroed); err != nil {
		t.Errorf("validateConfig(-probe-top 0 -limit 0) = %v, want nil", err)
	}

	cases := []struct {
		name string
		flag string
		bad  func(*config)
	}{
		{"concurrency zero deadlocks screenAll", "-concurrency", func(c *config) { c.concurrency = 0 }},
		{"concurrency negative panics in make", "-concurrency", func(c *config) { c.concurrency = -1 }},
		{"probe-bytes zero degrades every probe", "-probe-bytes", func(c *config) { c.probeBytes = 0 }},
		{"probe-bytes negative", "-probe-bytes", func(c *config) { c.probeBytes = -1 }},
		{"probe-time zero", "-probe-time", func(c *config) { c.probeTime = 0 }},
		{"timeout zero", "-timeout", func(c *config) { c.timeout = 0 }},
		{"probe-top negative", "-probe-top", func(c *config) { c.probeTop = -1 }},
		{"limit negative", "-limit", func(c *config) { c.limit = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.bad(cfg)
			err := validateConfig(cfg)
			if err == nil {
				t.Fatalf("validateConfig accepted %s", tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error = %q, want it to name %s", err, tc.flag)
			}
		})
	}
}

func TestResolveReferenceFallsBackToFreshestMirror(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mirrors := []*Mirror{
		{URL: "http://a/", Released: now.Add(-5 * time.Hour)},
		{URL: "http://b/", Released: now.Add(-1 * time.Hour)},
	}
	if got := resolveReference(now, mirrors); !got.Equal(now) {
		t.Errorf("resolveReference(known) = %v, want the archive's own date %v", got, now)
	}
	got := resolveReference(time.Time{}, mirrors)
	if want := now.Add(-1 * time.Hour); !got.Equal(want) {
		t.Errorf("resolveReference(zero) = %v, want the freshest mirror %v", got, want)
	}
}

func TestApplyBehind(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stale := &Mirror{URL: "http://a/", Released: now.Add(-5 * time.Hour)}
	ahead := &Mirror{URL: "http://b/", Released: now.Add(time.Hour)}
	unknown := &Mirror{URL: "http://c/"}
	applyBehind([]*Mirror{stale, ahead, unknown}, now)

	if stale.Behind != 5*time.Hour {
		t.Errorf("Behind = %v, want 5h", stale.Behind)
	}
	if ahead.Behind != 0 {
		t.Errorf("Behind = %v, want 0: a mirror ahead of the reference is not negative-behind", ahead.Behind)
	}
	if unknown.Behind != 0 {
		t.Errorf("Behind = %v, want 0 for a mirror with no release date", unknown.Behind)
	}
}

func TestServesArch(t *testing.T) {
	cases := []struct {
		url  string
		arch string
		want bool
	}{
		// The DE mirror list really does offer this one, and it carries no
		// binary-amd64 at all.
		{"http://ftp.tu-chemnitz.de/pub/linux/ubuntu-ports/", "amd64", false},
		{"http://ports.ubuntu.com/ubuntu-ports/", "amd64", false},
		{"http://ftp.tu-chemnitz.de/pub/linux/ubuntu/", "amd64", true},
		{"http://archive.ubuntu.com/ubuntu/", "amd64", true},
		// The mirror image of the same rule.
		{"http://ftp.tu-chemnitz.de/pub/linux/ubuntu-ports/", "arm64", true},
		{"http://ports.ubuntu.com/ubuntu-ports/", "riscv64", true},
		{"http://archive.ubuntu.com/ubuntu/", "arm64", false},
	}
	for _, c := range cases {
		if got := servesArch(c.url, c.arch); got != c.want {
			t.Errorf("servesArch(%q, %q) = %v, want %v", c.url, c.arch, got, c.want)
		}
	}
}

func TestSchemeAllowed(t *testing.T) {
	const httpURL, httpsURL = "http://example.org/ubuntu/", "https://example.org/ubuntu/"
	cases := []struct {
		url, want string
		allowed   bool
	}{
		{httpURL, "", true},
		{httpsURL, "", true},
		{httpURL, schemeAny, true},
		{httpsURL, schemeAny, true},
		{httpURL, schemeHTTP, true},
		{httpsURL, schemeHTTP, false},
		{httpURL, schemeHTTPS, false},
		{httpsURL, schemeHTTPS, true},
	}
	for _, c := range cases {
		if got := schemeAllowed(c.url, c.want); got != c.allowed {
			t.Errorf("schemeAllowed(%q, %q) = %v, want %v", c.url, c.want, got, c.allowed)
		}
	}
}

func TestValidateConfigRejectsUnknownScheme(t *testing.T) {
	base := func() *config {
		return &config{concurrency: 1, probeBytes: 1, probeTime: time.Second, timeout: time.Second}
	}
	// "" is the zero config and must stay valid; a real TestValidateConfig
	// regression here is how this was caught the first time.
	for _, s := range []string{"", schemeAny, schemeHTTP, schemeHTTPS} {
		cfg := base()
		cfg.scheme = s
		if err := validateConfig(cfg); err != nil {
			t.Errorf("validateConfig(-scheme %q) = %v, want nil", s, err)
		}
	}
	cfg := base()
	cfg.scheme = "ftp"
	if err := validateConfig(cfg); err == nil {
		t.Error("validateConfig accepted -scheme ftp")
	}
}

func TestProgressWritesAnUnprefixedLineToItsWriter(t *testing.T) {
	var buf bytes.Buffer
	// The banner names the program once, so status lines carry no prefix.
	progressTo(&buf, "screening %d candidates...", 20)
	got := buf.String()
	if got != "screening 20 candidates...\n" {
		t.Errorf("progressTo wrote %q", got)
	}
}

func TestMeasureScope(t *testing.T) {
	if got := measureScope(&config{probeTop: 5}); !strings.Contains(got, "5") {
		t.Errorf("measureScope(5) = %q, want it to name the count", got)
	}
	if got := measureScope(&config{probeTop: 0}); !strings.Contains(got, "every") {
		t.Errorf("measureScope(0) = %q, want it to say every candidate", got)
	}
}

func TestIsArchiveURL(t *testing.T) {
	// Every accepted case is a real URL from launchpad.net/ubuntu/+archivemirrors.
	// The nested ones are what the old shallow pattern dropped.
	accept := []string{
		"https://ftp.uni-stuttgart.de/ubuntu/",
		"http://ftp.icm.edu.pl/pub/Linux/ubuntu/",
		"http://cesium.di.uminho.pt/pub/ubuntu-archive/",
		"http://ftp.arnes.si/pub/mirrors/ubuntu/",
		"http://ftp.hosteurope.de/mirror/archive.ubuntu.com/",
		"http://ftp.nluug.nl/os/Linux/distr/ubuntu/",
		"http://ftp.rz.tu-bs.de/pub/mirror/ubuntu-packages/",
		"http://ports.ubuntu.com/ubuntu-ports/",
		// Real mirrors with unusual directory names. The old regexp matched a
		// prefix of these and emitted a truncated URL that always 404s.
		"http://artfiles.org/ubuntu.com/",
		"https://mirror.bacloud.com/ubuntu-mirror/archive/",
		"https://mirror.lyrahosting.com/ubuntuarchive/",
	}
	reject := []string{
		// apt cannot fetch over these: rsync is a mirror-administration
		// protocol and apt dropped its ftp method in 2.0.
		"rsync://ftp.uni-stuttgart.de/ubuntu/",
		"ftp://ftp.halifax.rwth-aachen.de/ubuntu/",
		// Launchpad's own chrome, linked from the same page.
		"https://documentation.ubuntu.com/launchpad/",
		"https://launchpad.net/ubuntu/+archivemirrors",
		"https://ubuntu.com/",
		"http://ppa.launchpadcontent.net/git-core/ppa/ubuntu",
	}
	for _, u := range accept {
		if !isArchiveURL(u) {
			t.Errorf("isArchiveURL(%q) = false, want true", u)
		}
	}
	for _, u := range reject {
		if isArchiveURL(u) {
			t.Errorf("isArchiveURL(%q) = true, want false", u)
		}
	}
}

func TestLaunchpadMirrorURLsKeepsEveryUsableScheme(t *testing.T) {
	// Shaped like the real listing: each mirror links its http, https and
	// rsync forms, and Launchpad's own pages are linked alongside.
	page := []byte(`
<a href="/ubuntu/+mirror/ftp.uni-stuttgart.de-archive">Stuttgart</a>
<a href="http://ftp.uni-stuttgart.de/ubuntu/">http</a>
<a href="https://ftp.uni-stuttgart.de/ubuntu/">https</a>
<a href="rsync://ftp.uni-stuttgart.de/ubuntu/">rsync</a>
<a href="http://ftp.icm.edu.pl/pub/Linux/ubuntu/">deep path</a>
<a href="https://documentation.ubuntu.com/launchpad/">docs</a>
<a href="http://ftp.uni-stuttgart.de/ubuntu/">duplicate</a>
`)
	got := parseLaunchpadMirrors(page)
	want := []string{
		"http://ftp.uni-stuttgart.de/ubuntu/",
		"https://ftp.uni-stuttgart.de/ubuntu/",
		"http://ftp.icm.edu.pl/pub/Linux/ubuntu/",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d URLs %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].URL != want[i] {
			t.Errorf("URL %d = %q, want %q", i, got[i].URL, want[i])
		}
	}
}

// The Launchpad listing is the one discovery fetch large enough that the base
// per-request timeout would cut its body off on a slow link, taking every ports
// mirror and https variant with it. It gets launchpadBudget instead.
func TestFetchLaunchpadMirrorsOutlastsTheBaseTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	// Well past one timeout, well inside launchpadBudget.
	const stall = 2 * timeout

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response writer does not support flushing")
			return
		}
		_, _ = io.WriteString(w, `<a href="http://ftp.uni-stuttgart.de/ubuntu/">http</a>`)
		flusher.Flush()
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `<a href="https://ftp.uni-stuttgart.de/ubuntu/">https</a>`)
	}))
	t.Cleanup(srv.Close)

	got, err := fetchLaunchpadMirrors(t.Context(), newTestClient(timeout), srv.URL, timeout)
	if err != nil {
		t.Fatalf("fetch: %v; the listing body must get launchpadBudget (%v), not the base timeout (%v)",
			err, launchpadBudget(timeout), timeout)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mirrors %v, want both halves of the page", len(got), got)
	}
}

func TestLaunchpadCountryTableIsWellFormed(t *testing.T) {
	if len(launchpadCountryNames) < 80 {
		t.Fatalf("country table has only %d entries", len(launchpadCountryNames))
	}
	seen := map[string]string{}
	for code, name := range launchpadCountryNames {
		if len(code) != 2 || strings.ToUpper(code) != code {
			t.Errorf("code %q is not a two-letter uppercase code", code)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("country %q mapped from both %s and %s", name, prev, code)
		}
		seen[name] = code
	}
	// Spot-check the inverted ISO forms Launchpad uses, which are the ones a
	// naive table would get wrong.
	for code, want := range map[string]string{
		"DE": "Germany", "PL": "Poland", "GB": "United Kingdom", "US": "United States",
		"KR": "Korea, Republic of", "VN": "Viet Nam", "RU": "Russian Federation",
		"IR": "Iran, Islamic Republic of", "TW": "Taiwan",
	} {
		if got := launchpadCountryNames[code]; got != want {
			t.Errorf("launchpadCountryNames[%q] = %q, want %q", code, got, want)
		}
	}
}

func TestLaunchpadMirrorsTagsEachMirrorWithItsCountry(t *testing.T) {
	page := []byte(`
<tr><th colspan="2">Germany</th></tr>
<tr><td><a href="https://ftp.uni-stuttgart.de/ubuntu/">https</a>
        <a href="http://ftp.uni-stuttgart.de/ubuntu/">http</a>
        <a href="rsync://ftp.uni-stuttgart.de/ubuntu/">rsync</a></td></tr>
<tr><th colspan="2">Poland</th></tr>
<tr><td><a href="http://ftp.icm.edu.pl/pub/Linux/ubuntu/">http</a></td></tr>
`)
	got := parseLaunchpadMirrors(page)
	want := []lpMirror{
		{URL: "https://ftp.uni-stuttgart.de/ubuntu/", Country: "DE"},
		{URL: "http://ftp.uni-stuttgart.de/ubuntu/", Country: "DE"},
		{URL: "http://ftp.icm.edu.pl/pub/Linux/ubuntu/", Country: "PL"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mirrors %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mirror %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestValidateConfigAcceptsCountriesLaunchpadDoesNotKnow(t *testing.T) {
	base := func() *config {
		return &config{concurrency: 1, probeBytes: 1, probeTime: time.Second, timeout: time.Second}
	}
	// Launchpad groups mirrors under 84 country headings, but the geo service
	// knows many more codes: MX alone has 99 mirrors there. Now that Launchpad
	// is always consulted, an unmapped code must degrade to the geo list with a
	// warning rather than fail the run.
	for _, code := range []string{"zz", "mx", "de"} {
		cfg := base()
		cfg.country = code
		if err := validateConfig(cfg); err != nil {
			t.Errorf("validateConfig(-country %s) = %v, want nil", code, err)
		}
	}
	// A malformed code is still an error: that is a typo, not a country.
	cfg := base()
	cfg.country = "deu"
	if err := validateConfig(cfg); err == nil {
		t.Error("validateConfig accepted -country deu")
	}
}

func TestParseCountries(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"de", []string{"DE"}, false},
		{"de,pl,cz", []string{"DE", "PL", "CZ"}, false},
		{" de , PL ", []string{"DE", "PL"}, false},
		{"de,de,pl", []string{"DE", "PL"}, false}, // deduped, order kept
		{"de,,pl", nil, true},                     // an empty entry is a typo, not "all"
		{"deu", nil, true},
		{"d", nil, true},
		{"d1", nil, true},
	}
	for _, c := range cases {
		got, err := parseCountries(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseCountries(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCountries(%q): %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseCountries(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("parseCountries(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestValidateConfigChecksEveryCountryInTheList(t *testing.T) {
	base := func() *config {
		return &config{concurrency: 1, probeBytes: 1, probeTime: time.Second, timeout: time.Second}
	}
	cfg := base()
	cfg.country = "de,pl"
	if err := validateConfig(cfg); err != nil {
		t.Errorf("validateConfig(de,pl) = %v, want nil", err)
	}
	// A malformed entry is rejected even without -launchpad.
	cfg = base()
	cfg.country = "de,xyz"
	if err := validateConfig(cfg); err == nil {
		t.Error("validateConfig accepted -country de,xyz")
	}
	// A code Launchpad does not group mirrors under is fine: Launchpad simply
	// contributes nothing for it and the geo list still applies.
	cfg = base()
	cfg.country = "de,zz"
	if err := validateConfig(cfg); err != nil {
		t.Errorf("validateConfig(de,zz) = %v, want nil", err)
	}
}

func TestResolveReferenceIgnoresFailedMirrors(t *testing.T) {
	fresh := time.Now().UTC()
	stale := fresh.Add(-48 * time.Hour)
	// A mirror that failed index verification keeps its Released date, but it
	// must not become the yardstick every other mirror's BEHIND is measured
	// against.
	failed := &Mirror{URL: "http://bad/", Released: fresh, Err: "index verification failed: x"}
	good := &Mirror{URL: "http://good/", Reachable: true, Released: stale}
	got := resolveReference(time.Time{}, []*Mirror{failed, good})
	if !got.Equal(stale) {
		t.Errorf("resolveReference = %v, want the good mirror's %v", got, stale)
	}
}

func TestResolveReferencePrefersTheFreshestEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fresh := now.Add(-10 * time.Minute)
	stale := now.Add(-30 * time.Hour)

	// archive.ubuntu.com is a pool. A backend that is mid-sync answers
	// successfully with the previous, older tree, and that stale date used to
	// win outright because the candidate scan only ran when the fetch failed.
	// Every mirror ahead of it then floored to zero and read "current", so the
	// column silently stopped discriminating.
	mirrors := []*Mirror{
		{URL: "http://a/", Reachable: true, Released: fresh},
		{URL: "http://b/", Reachable: true, Released: stale},
	}
	if got := resolveReference(stale, mirrors); !got.Equal(fresh) {
		t.Errorf("resolveReference(stale archive) = %v, want the fresher mirror %v", got, fresh)
	}
	// When the archive is the freshest thing we have, it still wins.
	archive := now.Add(-1 * time.Minute)
	if got := resolveReference(archive, mirrors); !got.Equal(archive) {
		t.Errorf("resolveReference(fresh archive) = %v, want %v", got, archive)
	}
}

func TestResolveReferenceIgnoresDatesInTheFuture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	honest := now.Add(-2 * time.Hour)
	// InRelease is signed but aptmir does not verify the signature, so a mirror
	// serving a forged future date would otherwise become the yardstick, make
	// every honest mirror look stale, and hand itself the top spot.
	forged := now.Add(72 * time.Hour)

	mirrors := []*Mirror{
		{URL: "http://honest/", Reachable: true, Released: honest},
		{URL: "http://forged/", Reachable: true, Released: forged},
	}
	if got := resolveReference(time.Time{}, mirrors); !got.Equal(honest) {
		t.Errorf("resolveReference = %v, want the honest mirror %v, not a future date", got, honest)
	}
	// The same guard applies to the archive's own answer.
	if got := resolveReference(forged, mirrors); !got.Equal(honest) {
		t.Errorf("resolveReference(future archive) = %v, want %v", got, honest)
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	s := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	for _, c := range []struct {
		p    int
		want float64
	}{{50, 50}, {90, 90}, {95, 100}, {100, 100}, {1, 10}} {
		if got := percentile(s, c.p); got != c.want {
			t.Errorf("percentile(p%d) = %v, want %v", c.p, got, c.want)
		}
	}
	// A single sample is its own every percentile.
	if got := percentile([]float64{7}, 90); got != 7 {
		t.Errorf("percentile of one sample = %v, want 7", got)
	}
}

func TestSummarize(t *testing.T) {
	if _, ok := summarize(nil); ok {
		t.Error("summarize(nil) reported a summary")
	}
	got, ok := summarize([]float64{40, 10, 30, 20})
	if !ok {
		t.Fatal("summarize returned no summary")
	}
	if got.n != 4 || got.min != 10 || got.max != 40 {
		t.Errorf("n/min/max = %d/%v/%v, want 4/10/40", got.n, got.min, got.max)
	}
	if got.mean != 25 {
		t.Errorf("mean = %v, want 25", got.mean)
	}
	if got.p50 != 20 {
		t.Errorf("p50 = %v, want 20", got.p50)
	}
}

func TestPrintSummarySeparatesAndCountsSamples(t *testing.T) {
	mirrors := []*Mirror{
		{URL: "http://a/", Response: 10 * time.Millisecond, Bandwidth: 10e6},
		{URL: "http://b/", Response: 20 * time.Millisecond, Bandwidth: 30e6},
		// Screened but never measured for bandwidth: it counts toward RESPONSE
		// only, which is why the two rows report their own sample counts.
		{URL: "http://c/", Response: 30 * time.Millisecond},
		// Never answered at all: counts toward neither.
		{URL: "http://d/"},
	}
	var buf bytes.Buffer
	printSummary(&buf, mirrors)
	out := buf.String()

	if !strings.HasPrefix(out, "\n") {
		t.Errorf("summary is not separated from the table by a blank line: %q", out[:20])
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want blank + header + 2 rows:\n%s", len(lines), out)
	}
	for _, want := range []string{"MIN", "P50", "MEAN", "P90", "P95", "MAX", "SAMPLES"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("header lacks %s: %q", want, lines[1])
		}
	}
	if !strings.HasPrefix(lines[2], "RESPONSE") || !strings.HasSuffix(lines[2], "3") {
		t.Errorf("RESPONSE row = %q, want 3 samples", lines[2])
	}
	if !strings.HasPrefix(lines[3], "BANDWIDTH") || !strings.HasSuffix(lines[3], "2") {
		t.Errorf("BANDWIDTH row = %q, want 2 samples", lines[3])
	}
}

func TestPrintSummaryEmitsNothingWithoutMeasurements(t *testing.T) {
	var buf bytes.Buffer
	printSummary(&buf, []*Mirror{{URL: "http://a/"}})
	if buf.Len() != 0 {
		t.Errorf("printSummary wrote %q for a run with nothing measured", buf.String())
	}
}

func TestCSPMirrors(t *testing.T) {
	got := cspMirrors("amd64")
	// Azure plus both schemes for every AWS region.
	want := 1 + 2*len(ec2Regions)
	if len(got) != want {
		t.Fatalf("got %d mirrors, want %d", len(got), want)
	}

	var azureSchemes, ec2Schemes []string
	for _, u := range got {
		switch {
		case strings.Contains(u, "azure."):
			azureSchemes = append(azureSchemes, strings.SplitN(u, ":", 2)[0])
		case strings.Contains(u, ".ec2."):
			ec2Schemes = append(ec2Schemes, strings.SplitN(u, ":", 2)[0])
		default:
			t.Errorf("unexpected mirror %q", u)
		}
		// They must survive the filters every other candidate passes.
		if !servesArch(u, "amd64") {
			t.Errorf("%q does not serve amd64", u)
		}
		if normalizeMirrorURL(u) == "" {
			t.Errorf("%q does not normalise", u)
		}
	}
	// Azure answers nothing on TLS for that name, so offering https would only
	// produce a permanently unreachable row.
	if len(azureSchemes) != 1 || azureSchemes[0] != "http" {
		t.Errorf("azure schemes = %v, want [http] only", azureSchemes)
	}
	if len(ec2Schemes) != 2*len(ec2Regions) {
		t.Errorf("got %d ec2 entries, want both schemes per region", len(ec2Schemes))
	}
}

func TestCSPMirrorsSkipsPortsArchitectures(t *testing.T) {
	// Neither provider publishes a ports tree: both answer 404 for
	// /ubuntu-ports/. Offering them on a ports run would add candidates that
	// cannot serve the architecture at all.
	for _, arch := range []string{"arm64", "riscv64", "ppc64el"} {
		if got := cspMirrors(arch); got != nil {
			t.Errorf("cspMirrors(%q) returned %d mirrors, want none", arch, len(got))
		}
	}
}

func TestEC2RegionsAreWellFormed(t *testing.T) {
	if len(ec2Regions) < 20 {
		t.Errorf("only %d regions listed", len(ec2Regions))
	}
	seen := map[string]bool{}
	for _, r := range ec2Regions {
		if seen[r] {
			t.Errorf("region %q listed twice", r)
		}
		seen[r] = true
		if strings.ContainsAny(r, "./ ") || r == "" {
			t.Errorf("region %q is not a bare region name", r)
		}
	}
}

func TestVersionStringNamesTheProgram(t *testing.T) {
	// -version prints this verbatim, and the banner reuses it, so the shape is
	// what a user sees in both places.
	got := versionString()
	if !strings.HasPrefix(got, "aptmir ") {
		t.Errorf("version string %q does not start with the program name", got)
	}
	for _, want := range []string{"commit", "built"} {
		if !strings.Contains(got, want) {
			t.Errorf("version string %q does not mention %s", got, want)
		}
	}
	// An unreleased build reports dev rather than an empty version.
	if version == "" {
		t.Error("Version() is empty; a build outside the release process should report dev")
	}
}

func TestIsPoolHost(t *testing.T) {
	// Canonical's rotation names. Measured: archive.ubuntu.com answers from 9
	// addresses, security and pl.archive from 9, us-east-1.ec2 from 10, and
	// azure resolves through Azure Traffic Manager. A request lands on whichever
	// backend answers, and they are not always in the same state — the
	// Archive-Update-in-Progress marker showed on 2 of 10 requests to
	// archive.ubuntu.com, so its status flapped between runs.
	pools := []string{
		"http://archive.ubuntu.com/ubuntu/",
		"http://security.ubuntu.com/ubuntu/",
		"http://ports.ubuntu.com/ubuntu-ports/",
		"http://pl.archive.ubuntu.com/ubuntu/",
		"http://de.archive.ubuntu.com/ubuntu/",
		"http://azure.archive.ubuntu.com/ubuntu/",
		"http://us-east-1.ec2.archive.ubuntu.com/ubuntu/",
		"https://archive.ubuntu.com/ubuntu/",
	}
	// Real mirrors: one address, one operator, one tree.
	single := []string{
		"http://ftp.psnc.pl/linux/ubuntu/",
		"http://ftp.icm.edu.pl/pub/Linux/ubuntu/",
		"http://mirror.informatik.tu-freiberg.de/ubuntu/",
		"http://ftp.hosteurope.de/mirror/archive.ubuntu.com/",
	}
	for _, u := range pools {
		if !isPoolHost(u) {
			t.Errorf("isPoolHost(%q) = false, want true", u)
		}
	}
	for _, u := range single {
		if isPoolHost(u) {
			t.Errorf("isPoolHost(%q) = true, want false", u)
		}
	}
}

func TestPrintTableMarksPoolAndCDNSeparately(t *testing.T) {
	now := time.Now()
	mirrors := []*Mirror{
		{URL: "http://archive.ubuntu.com/ubuntu/", Reachable: true, Released: now, Pool: true},
		{URL: "http://edge.example/ubuntu/", Reachable: true, Released: now, CDN: "cloudflare"},
		{URL: "http://both.example/ubuntu/", Reachable: true, Released: now, Pool: true, CDN: "fastly"},
		{URL: "http://plain.example/ubuntu/", Reachable: true, Released: now},
	}
	var buf bytes.Buffer
	printTable(&buf, mirrors)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[1:]

	for i, want := range []string{"ok (pool)", "ok (via cloudflare)", "ok (pool) (via fastly)", "ok"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("row %d status = %q, want it to end with %q", i+1, lines[i], want)
		}
	}
	// The plain mirror must not pick up either marker.
	if strings.Contains(lines[3], "pool") || strings.Contains(lines[3], "via") {
		t.Errorf("plain mirror was marked: %q", lines[3])
	}
}

// withDebug points the tracer at a buffer for the duration of a test.
func withDebug(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	enableDebug(&buf)
	t.Cleanup(disableDebug)
	return &buf
}

func TestTraceIsSilentUntilEnabled(t *testing.T) {
	var buf bytes.Buffer
	t.Cleanup(disableDebug)

	debugLog = slog.New(slog.NewTextHandler(&buf, nil))
	debugLog.Debug("request", "method", "GET")
	if buf.Len() != 0 {
		t.Errorf("traced %q without -d", buf.String())
	}
	if debugging() {
		t.Error("debugging() is true without -d")
	}

	enableDebug(&buf)
	if !debugging() {
		t.Error("debugging() is false after enableDebug")
	}
}

func TestTraceIsLogfmt(t *testing.T) {
	buf := withDebug(t)
	debugLog.Debug("request", "method", "GET", "url", "http://example/ubuntu/",
		"status", 200, "dur", 98*time.Millisecond)

	line := strings.TrimRight(buf.String(), "\n")
	// logfmt: space-separated key=value, values quoted only when they need it.
	for _, want := range []string{
		"level=DEBUG", "msg=request", "method=GET",
		"url=http://example/ubuntu/", "status=200", "dur=98ms",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if !strings.HasPrefix(line, "time=") {
		t.Errorf("line %q does not start with a timestamp", line)
	}
	// A message with spaces has to be quoted or the encoding breaks.
	buf.Reset()
	debugLog.Debug("marker check failed", "err", "TLS handshake timeout")
	if got := buf.String(); !strings.Contains(got, `msg="marker check failed"`) ||
		!strings.Contains(got, `err="TLS handshake timeout"`) {
		t.Errorf("multi-word values not quoted: %q", got)
	}
}

func TestTraceSurvivesConcurrentWriters(t *testing.T) {
	// Screening and the sweep trace from -concurrency goroutines at once.
	buf := withDebug(t)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			debugLog.Debug("probe", "mirror", fmt.Sprintf("http://mirror-%02d/ubuntu/", i), "ttfb", time.Millisecond)
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	for _, l := range lines {
		if !strings.Contains(l, "msg=probe") || !strings.HasSuffix(l, "ttfb=1ms") {
			t.Errorf("interleaved line: %q", l)
		}
	}
}

func TestEverythingAboveTheResultsIsLogfmtUnderDebug(t *testing.T) {
	// -d makes the whole stderr stream parseable: banner, phase announcements
	// and trace alike. The results stay plain text on stdout.
	buf := withDebug(t)
	announce("discovering mirrors for resolute/amd64...", "discovering mirrors",
		"codename", "resolute", "arch", "amd64")
	blankLine()
	announce("measuring response time to 6 candidates...", "response sweep", "candidates", 6)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 — a blank line is not logfmt and must be skipped:\n%s",
			len(lines), buf.String())
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "time=") || !strings.Contains(l, "level=DEBUG msg=") {
			t.Errorf("not logfmt: %q", l)
		}
	}
	if !strings.Contains(lines[0], `msg="discovering mirrors" codename=resolute arch=amd64`) {
		t.Errorf("announcement lost its fields: %q", lines[0])
	}
}

func TestAnnounceIsPlainProseWithoutDebug(t *testing.T) {
	// Without -d the same call is the sentence a person reads.
	var buf bytes.Buffer
	prev := progressOut
	progressOut = &buf
	t.Cleanup(func() { progressOut = prev })
	disableDebug()

	progressTo(&buf, "%s", "discovering mirrors for resolute/amd64...")
	if got := buf.String(); got != "discovering mirrors for resolute/amd64...\n" {
		t.Errorf("plain announcement = %q", got)
	}
}

func TestVersionStringCarriesEachField(t *testing.T) {
	// The banner logs these as separate keys under -d, so each has to exist in
	// its own right and appear in the formatted string.
	for name, got := range map[string]string{"version": version, "commit": commit, "built": built} {
		if got == "" {
			t.Errorf("%s is empty", name)
		}
		if !strings.Contains(versionString(), got) {
			t.Errorf("%s = %q, absent from versionString() %q", name, got, versionString())
		}
	}
}

// parseArgs binds every flag onto a throwaway FlagSet and parses args into it.
func parseArgs(t *testing.T, args ...string) *config {
	t.Helper()
	cfg := &config{}
	fs := flag.NewFlagSet("aptmir", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cfg
}

func TestEveryFlagIsBound(t *testing.T) {
	// -debug went a whole release unbound: it was documented, accepted by
	// nobody and reported by nothing. Both spellings of every paired flag are
	// checked here so that cannot recur silently.
	for _, spelling := range []string{"-v", "-version"} {
		if !parseArgs(t, spelling).showVersion {
			t.Errorf("%s did not set showVersion", spelling)
		}
	}
	for _, spelling := range []string{"-d", "-debug"} {
		if !parseArgs(t, spelling).debug {
			t.Errorf("%s did not set debug", spelling)
		}
	}
	// A representative flag of each kind, so a mistyped binding is caught.
	if got := parseArgs(t, "-country", "de,pl").country; got != "de,pl" {
		t.Errorf("-country = %q", got)
	}
	if got := parseArgs(t, "-screen-top", "7").screenTop; got != 7 {
		t.Errorf("-screen-top = %d", got)
	}
	if got := parseArgs(t, "-timeout", "5s").timeout; got != 5*time.Second {
		t.Errorf("-timeout = %v", got)
	}
	if !parseArgs(t, "-no-cache").noCache || !parseArgs(t, "-include-csp").includeCSP {
		t.Error("a boolean flag did not bind")
	}
}

func TestDefaultsMatchTheDocumentedOnes(t *testing.T) {
	// The README states these; a silent change to either side should fail here
	// rather than mislead a reader.
	cfg := parseArgs(t)
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"-concurrency", cfg.concurrency, 30},
		{"-screen-top", cfg.screenTop, 50},
		{"-probe-top", cfg.probeTop, 10},
		{"-limit", cfg.limit, 0},
		{"-probe-bytes", cfg.probeBytes, int64(measureBytes)},
		{"-max-age", cfg.maxAge, 24 * time.Hour},
		{"-scheme", cfg.scheme, schemeAny},
		{"-no-cache", cfg.noCache, false},
	} {
		if c.got != c.want {
			t.Errorf("%s default = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A mirror's own release file names the index paths, and a verification failure
// puts one of those paths in the STATUS column. That text is written by whoever
// runs the mirror, so it reaches the terminal only after the characters that
// could drive it are removed: an escape sequence could recolour or erase the
// rest of the table, and a newline could forge an extra row.
func TestPrintTableStripsControlCharactersFromMirrorText(t *testing.T) {
	hostile := "index verification failed: \x1b[2Kdists/\nnoble/fake\x07 ok"
	_, rows := tableRows(t, []*Mirror{
		{URL: "http://mirror.example/ubuntu/", Err: hostile},
	})
	row := rows[0]
	for _, bad := range []string{"\x1b", "\x07"} {
		if strings.Contains(row, bad) {
			t.Errorf("row kept control character %q: %q", bad, row)
		}
	}
	// tableRows already fails if the newline forged a second row, but say why.
	if strings.Contains(row, "\n") {
		t.Errorf("row kept a newline: %q", row)
	}
	// The diagnostic itself must survive: stripping is not censoring.
	if !strings.Contains(row, "dists/") || !strings.Contains(row, "noble/fake") {
		t.Errorf("row lost the diagnostic text: %q", row)
	}
}
