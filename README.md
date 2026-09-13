# aptmir

Ranks Ubuntu archive mirrors by measured freshness and throughput, and reports
what it found. It covers the ranking half of `apt-smart` and understands the
deb822 sources format Ubuntu has shipped by default since 24.04, so it works on
22.04 through 26.04 and later. Changing your sources is left to you.

Standard library only — no dependencies, nothing to break.

## Platforms

Linux is the target: the tool reads `/etc/os-release` and `/etc/apt`, and shells
out to `dpkg` and `lsb_release`. Releases also carry macOS binaries for
development convenience, where autodetection cannot work and `-codename` and
`-arch` have to be given explicitly. Windows is not built, having none of the
above.

## Installation

Pre-built binaries for Linux and macOS are available from the
[GitHub releases page](https://github.com/krisiasty/aptmir/releases).

On macOS, install the Homebrew cask:

```sh
brew install --cask krisiasty/tap/aptmir
```

## Usage

Rank mirrors:

```sh
aptmir
```

Restrict to specific countries:

```sh
aptmir -country pl,de
```

Measure the cold path using only mirrors published over HTTP:

```sh
aptmir -no-cache -scheme http
```

aptmir never writes to your system. It reports; changing `/etc/apt` is
your decision and your edit, so it needs no root and no `sudo`.

Run `aptmir --help` for the full list of options.

## What it measures

Probing happens in three phases, each narrowing the field for the next.

| Phase | Candidates | What it does | Bound by |
| --- | --- | --- | --- |
| 1. Response | all | Time to first byte, best of three probes | `-concurrency` |
| 2. Screen | closest | Freshness and sync-lock status | `-screen-top`, `-concurrency` |
| 3. Bandwidth | fastest | Sustained throughput, one mirror at a time | `-probe-top` |

**Phase 1 measures time to first byte, not round-trip latency.** A TCP handshake
terminates at the nearest CDN edge, so it measures the distance to a point of
presence rather than to the mirror: a Hong Kong mirror behind Cloudflare
answered a handshake in 14 ms while its first byte took 691 ms. Ranked on
handshakes it displaced genuinely fast mirrors out of the field entirely. Each
candidate gets three probes and the best is kept; see
[Why response time, not ping](#why-response-time-not-ping) for why three.

**Phase 2 screens the `-screen-top` closest** (50 by default) for freshness —
the `Date:` field of their `InRelease` — and for an `Archive-Update-in-Progress`
sync lock. These checks are cheap, but the phase is not uniformly cheap: a
candidate that turns out to carry a lock also has its indexes verified here,
which pulls roughly 31 MB from that one mirror, and up to `-concurrency` of
those can be in flight at once. Verification stays in this phase because it is a
safety gate — a marked mirror must not reach the ranking until its tree has been
checked.

**Phase 3 measures bandwidth** on the `-probe-top` fastest (10 by default), one
mirror at a time rather than concurrently: concurrent downloads compete for your
own uplink and randomise the result, which is what once made the ranking change
from run to run. `-probe-top 0` measures every usable candidate instead.

Staleness is decided between phases 2 and 3, so a mirror the ranking would
reject as too far behind never consumes one of the few measured slots.

### Which candidates are considered

`mirrors.ubuntu.com` returns mirrors for the main archive and the ports archive
in the same list, and the two are published under different paths. A ports tree
carries no `binary-amd64` at all, so candidates are filtered to those that can
actually serve the detected architecture — otherwise a ports mirror with a good
latency wins a measured slot and then reports itself unreachable.

Every candidate that survives those filters is screened. `-limit` exists only to
cap a pathologically long list and defaults to no cap, because screening is
cheap — one `InRelease` fetch of roughly 130 KB per mirror — while `-probe-top`
already bounds the expensive phase. Capping the list instead samples it: the
mirror list is returned in a shuffled order, so a `-limit` smaller than the list
discards a different arbitrary subset on every run, and the fastest mirror may
simply not be in the part that was kept.

Candidates also come from Launchpad's mirror listing, which is always consulted. It
names every scheme a mirror offers. Only `http` and `https` are taken from it:
apt cannot speak `rsync` at all, and it removed its `ftp` method in 2.0, so
those are mirror-administration protocols rather than usable sources. A mirror's
archive is recognised anywhere in the URL path, at any depth and in any case,
because mirrors name the directory as they please — `/pub/Linux/ubuntu/`,
`/mirror/archive.ubuntu.com/`, `/ubuntu-mirror/archive/` and `/ubuntuarchive/`
are all real examples from that page.

`-country` takes one code or a comma-separated list, so `-country pl,de,cz`
searches all three. The mirror service publishes a separate list per country, so
each code costs one extra list fetch; results are merged in the order given and
each mirror keeps the country it came from. An empty entry such as `de,,pl` is
rejected rather than read as "everywhere", because silently widening the search
is the mistake this flag exists to prevent.

`-country` narrows Launchpad too. The listing groups mirrors under country
headings, so each candidate is tagged with the country it appears under and
filtered like the geo list. Launchpad prints ISO 3166-1 English short names,
inversions included, so the code-to-name table is transcribed from the live page
rather than guessed — `Korea, Republic of`, not `South Korea`. It covers the 84
countries that currently host a mirror; a code outside that set is rejected with
reported as a warning: Launchpad simply contributes nothing for that country and
the geo list still applies. If the listing cannot be read at all, the run says so
loudly and continues on the geo list alone.

Combining the two sources is worthwhile: for Germany the geo list offers 42
candidates and Launchpad adds 16 more, including the `http` form of mirrors the
geo list publishes only over `https`.

There are exactly four sources of candidates, and none of them is you: the geo
list, the Launchpad listing, the cloud-provider table behind `-include-csp`, and
the canonical archive. No flag takes a URL, so there is no way to point aptmir at
a mirror of your own choosing, including one on your own network.

Of those four, only the geo list is fetched over plaintext HTTP.
`mirrors.ubuntu.com` publishes no HTTPS endpoint at all, so there is nothing
better to prefer; the Launchpad listing is fetched over HTTPS. An attacker on the
path to the geo service can therefore add a candidate, which is the reason the
`Safety` section below is worth reading before you paste a result into your
sources.

`-scheme` narrows the candidates further to `http` or `https` only. The default,
`any`, keeps both. Note that a mirror often appears under only one of the two, so
restricting the scheme can shrink the candidate pool noticeably, and `-scheme
https` also excludes the plain-HTTP canonical archive that is otherwise added as
a fallback.

### Why response time, not ping

The sweep measures time to first byte, not a TCP handshake, and the difference
decides the outcome. A handshake terminates at the nearest CDN edge: a Hong Kong
mirror behind Cloudflare answered one in 14 ms while its first byte took 691 ms.
Ranked on handshakes it beat a German mirror that answers in 36 ms, took a slot
in the screening cut, and pushed genuinely fast mirrors out of it — a worldwide
run picked a 21.9 MB/s mirror where the right answer was 56 MB/s.

The probe fetches one byte of the detached `Release` file with a unique query
string. `Release` has no extension, so a front end that caches by extension
leaves it alone, and the query gives a distinct cache key to any that does not.
Both matter: with a cacheable target, three probes of the same file went 675 ms,
57 ms, 48 ms, because the first probe warms the edge and the rest are served
from it. Taking the best of three would then report a cache entry aptmir created
itself. **If you ever point this probe at a cacheable file, best-of-three stops
being valid.**

Three probes rather than one, because they are not only about measuring: they
absorb a slow DNS answer or a momentary failure. Against the real list one probe
calls 33-59 hosts dead where three call 13-24, so roughly 20-40 live mirrors per
run would otherwise be discarded before anything else looked at them.

### Rotations are marked

`archive.ubuntu.com` and its relatives are not servers, they are rotations.
`archive.ubuntu.com` answers from nine addresses, `security.ubuntu.com` and
`pl.archive.ubuntu.com` from nine, `us-east-1.ec2.archive.ubuntu.com` from ten;
`azure.archive.ubuntu.com` rotates through Azure Traffic Manager instead. A real
mirror answers from one address.

That matters because the backends are not always in the same state. The
`Archive-Update-in-Progress` marker showed on 2 of 10 requests to
`archive.ubuntu.com`, and its status flapped between `ok` and `syncing` from one
run to the next as a result.

So a figure measured against one of these belongs to whichever backend answered,
not to a server you could pin in your sources, and repeating the run can give a
different answer for reasons that have nothing to do with the mirror. Such hosts
are marked `(pool)` in `STATUS` rather than sitting in the table looking like an
ordinary mirror. They are not demoted: `archive.ubuntu.com` is what Ubuntu ships
by default and is a perfectly reasonable choice.

Matching is on the hostname. `ftp.hosteurope.de` serves the archive from a path
containing `archive.ubuntu.com` and is a single server, so it is not marked.

### Caching front ends, and fleets

A mirror behind a CDN is marked in `STATUS`, for example `ok (via cloudflare)`,
because it inverts which mirror is best and the answer depends on how you use it.

Measured on one such mirror: 5.6 MB/s on a cold fetch, and 43-51 MB/s once the
edge was warm — beating a direct regional mirror. One machine fetching once pays
the cold path. A fleet pays it once and then everything else is served warm from
a nearby edge.

aptmir measures the warm path by default, because it is closer to what apt
actually does: apt fetches hundreds of files over reused connections, so a cold
first-fetch penalty is amortised rather than paid per file, and on a fleet the
edge is warm for everyone after the first machine. Probes are cache-friendly and
the bandwidth measurement primes the file before timing it, which roughly doubles
that phase's traffic.

`-no-cache` measures the cold path instead: every probe is forced past the cache
and the bandwidth figure is a single cold fetch. That is the honest number for a
one-off fetch on a single machine, and it ranks CDN-fronted mirrors far lower —
in a worldwide run they were 6 of the top 10 by default and 2 of 10 under
`-no-cache`.

Note that `-no-cache` is also what keeps best-of-three honest in the sweep. With
a cacheable target the first probe warms the edge and the next two are served
from it, so the default deliberately accepts that: it is measuring the warm path,
which is the point. Under `-no-cache` every probe reaches the origin, so the best
of the three is a real origin measurement.

### Cloud provider archives

Cloud providers run archive mirrors for their own instances. They are listed
neither on Launchpad nor in the geo lists, so `-include-csp` names them
directly: `azure.archive.ubuntu.com`, and every AWS region as
`<region>.ec2.archive.ubuntu.com`. That adds 57 candidates — Azure plus both
schemes for 28 regions.

There is no unprefixed `ec2.archive.ubuntu.com`, and nothing tells aptmir which
region a machine is in, so every region is offered and the response-time sweep
picks the nearest. From inside the matching region these are usually the best
choice available: on-network and free of egress charges.

Two others are deliberately absent. `gce.archive.ubuntu.com` does not resolve
publicly at all, and `oci.archive.ubuntu.com` resolves to Canonical's own
addresses from outside, so it is not a distinct mirror for anyone who could
reach it here.

`-include-csp` is not narrowed by `-country`: these are named rather than
discovered, and AWS regions do not map onto country codes. It also does nothing
on a ports architecture, because neither provider publishes a ports tree.

Contrary to a common belief, these mirrors **do** carry security updates.
Checked against `security.ubuntu.com` for both `noble-security` and
`resolute-security`, Azure and the AWS regions reported an identical `Date:` and
an identical SHA256, and the `Packages.gz` bytes matched.

### Tracing a run

`-d` (or `-debug`) traces every HTTP request with its status and timing, every
sweep probe and the best-of-three it produces, every release date read, every
index verified, every byte counted by the bandwidth phase, and what the
screening cut kept.

Under `-d` **everything above the results** is [logfmt](https://brandur.org/logfmt),
written by `log/slog`'s `TextHandler` — the banner and the phase announcements
as well as the trace itself, so the whole stderr stream parses as one format.
The blank lines that separate the status block are skipped, a bare newline not
being logfmt. The results keep their plain layout on stdout.

```text
time=2026-09-13T15:54:37Z level=DEBUG msg=request method=GET url=http://mirrors.ubuntu.com/PL.txt status=200 dur=92ms
time=2026-09-13T15:54:37Z level=DEBUG msg=probe mirror=http://ftp.psnc.pl/linux/ubuntu/ ttfb=8.07ms
time=2026-09-13T15:54:37Z level=DEBUG msg=response mirror=http://ftp.psnc.pl/linux/ubuntu/ best_of=3 ttfb=8.02ms
time=2026-09-13T15:54:38Z level=DEBUG msg="verify ok" url=http://ubuntu.man.lodz.pl/... bytes=20128002
time=2026-09-13T15:54:40Z level=DEBUG msg=pull url=http://ftp.psnc.pl/... timed_bytes=6291456 warmup_bytes=2097152
time=2026-09-13T15:54:40Z level=DEBUG msg=bandwidth mirror=http://ftp.psnc.pl/linux/ubuntu/ rate="42.2 MB/s" low_confidence=false
```

So `-d 2>&1 | grep msg=probe` gives every probe, and a `cdn=` key appears only
on mirrors that have a front end.

Two things the trace makes visible that the table cannot. The bandwidth phase
logs two `pull` lines per mirror by default, because caching is allowed and the
file is primed before being timed; under `-no-cache` there is one. And the three
probes are logged individually beside the `response` that summarises them, so a
mirror whose first probe was unlucky can be told from one that is genuinely slow.

The once-a-second `x/y` progress counts are suppressed under `-d`. The trace
already reports each probe, verification and measurement as it happens, so a
periodic count of them is noise laid over the detail it summarises. The phase
announcements stay, which keeps a long trace navigable.

### Output layout

The first line names the program and its version, then a blank line, then one
status line per phase, then a blank line before the results:

```text
aptmir 1.2.0 (commit a1b2c3d, built 2026-09-13T10:00:00Z)

discovering mirrors for resolute/amd64...
measuring response time to 115 candidates...
measured 115/115
screening 50 closest candidates (freshness, sync locks)...
screened 50/50
measuring bandwidth on the 10 fastest candidates, one at a time...
measured bandwidth on 10/10

#    MIRROR                                     BANDWIDTH  RESPONSE  ...
```

Everything above the results goes to stderr and everything below it to stdout,
including both blank lines, so redirecting stdout gives the table and the
summary and nothing else. `-version` prints the same version string on its own
to stdout and exits.

### Progress output

Each phase prints one short line to stderr as it starts, so a run that spends
half a minute probing does not look like it has hung. Results go to stdout, so
the table and `-json` stay clean when stderr is redirected away.

Bandwidth is measured by downloading a `Range` of the real `Contents-<arch>.gz`
index (falling back to the smaller `Packages.gz` when that index is
unavailable, which is flagged `(low-confidence)` in the STATUS column since a
smaller sample is a less trustworthy measurement), discarding an initial
warm-up before starting the clock. TCP's slow-start ramp-up otherwise makes a
short probe measure how fast the congestion window opened rather than what
the mirror can sustain, so the reported number reflects sustained throughput.
`-probe-bytes` sets how many bytes are timed in that measurement window,
after the warm-up is discarded; `-probe-time` bounds how long it may take.

`-timeout` bounds a complete ordinary HTTP request, including its response
body. Three phases need longer and are budgeted separately: screening may spend
up to nine times that value on one mirror because it can verify several large
indexes; bandwidth probes use `-probe-time` plus connection overhead; and the
Launchpad mirror listing, a few hundred kilobytes fetched once, gets three times
the timeout. All three remain bounded.

For each candidate mirror, the table reports:

| Column | Meaning |
| --- | --- |
| `BANDWIDTH` | Sustained bytes per second measured over `-probe-bytes` worth of data, after discarding a fixed warm-up. |
| `RESPONSE` | Fastest of three time-to-first-byte probes. This is what decides which candidates are screened at all. |
| `BEHIND` | Hours between this mirror's `InRelease` `Date:` field and the official archive's. Measured directly, not read off a status page. |
| `STATUS` | `syncing` when the `Archive-Update-in-Progress` marker is present and confirmed recent — the mirror is mid-rsync, so its tree can change under a reader even though it answers. It is still ranked and shown, demoted below every clean mirror. `stale-lock` when that marker is present but older than an hour, or its age could not be read at all — see below. `marker-unknown` when the mirror serves the archive but its root listing could not be checked. Otherwise `ok`, or the error that made the mirror unusable. Suffixed `(low-confidence)` when the bandwidth figure came from the smaller `Packages.gz` index, or from a transfer too short to measure past the warm-up. |

A `stale-lock` mirror carries a sync marker that is either older than an hour
or whose age could not be determined at all: a marker whose `Last-Modified`
is missing or unparsable is treated as stale rather than as an active sync,
so an unreadable age never gets the benefit of the doubt. Either way, the
sync that left the marker almost certainly died without cleaning up, rather
than one still running. Because a leaked lock can leave a tree mid-mutation,
such a mirror is verified against the hashes recorded in its own `InRelease`
before it is trusted at all; if verification fails it is reported as
unreachable instead. A mirror that passes is still real and usable, but
ranked below every clean mirror rather than on equal footing.

`marker-unknown` means the release metadata was readable but the archive root
could not be listed, commonly because directory listing is disabled and the
root answers 403 or 404, though a timeout or a reset reads the same way.
aptmir therefore cannot confirm whether a sync marker exists. The mirror
remains usable and is demoted, sharing the one demoted tier with `syncing` and
`stale-lock` mirrors rather than sitting below them. Its indexes are not
verified either: verification is what a marker triggers, and here no marker was
seen to trigger it. A marker that was seen but could not be confirmed, because
the confirming request failed rather than came back empty, is reported as
`stale-lock` and verified like any other lock of unreadable age.

Ranking is by throughput among mirrors that are reachable and no staler than
`-max-age` (24 h by default), with clean mirrors ranked ahead of `syncing`,
`stale-lock`, and `marker-unknown` ones. Ranking demotes these mirrors rather
than hiding them. Latency and bandwidth are close to uncorrelated for mirrors:
a nearby host on a saturated uplink loses to a more distant one behind a CDN,
which is why bandwidth leads.

Every measurement is a single snapshot on one network path. A mirror that
benchmarks well at 03:00 may be congested at 19:00.

## Summary

Below the table, separated by a blank line, each measured column is summarised:

```text
                  MIN        P50       MEAN        P90        P95        MAX  SAMPLES
RESPONSE         7 ms      18 ms      17 ms      22 ms      22 ms      22 ms  15
BANDWIDTH   19.3 MB/s  30.8 MB/s  34.1 MB/s  58.6 MB/s  58.6 MB/s  58.6 MB/s  6
```

Percentiles are nearest-rank rather than interpolated: these samples are small,
and an interpolated figure would imply a precision they do not carry.

The two rows carry separate sample counts because they are not drawn from the
same set. Every screened mirror has a response time, but only the `-probe-top`
fastest are measured for bandwidth, so with the default of five the bandwidth
`P90` is rarely distinguishable from `MAX`. `SAMPLES` is there to make that
visible rather than leave it implied. Raise `-probe-top` if you want the
bandwidth percentiles to mean something.

Note the two rows read in opposite directions: for `RESPONSE` lower is better,
for `BANDWIDTH` higher is.

## Safety

aptmir does not modify your system. It has no write path: it reads
`/etc/apt` only to detect your release codename, and everything else it
touches is a network fetch. Nothing it prints takes effect until you edit
your sources yourself.

Probes refuse to connect to a non-public address. Loopback, private, link-local,
carrier-grade NAT, unspecified and multicast destinations are rejected at the
moment the connection is dialled, which covers a literal address in the
catalogue, a listed mirror whose name resolves inside your network, and every
redirect hop along the way. This is defence in depth rather than a patched hole:
what would otherwise reach such an address is an unauthenticated `GET` or `HEAD`
carrying no credentials, with no query string, whose body is read to a bounded
limit and discarded. It exists mainly because a mirror resolving to a private
address needs no attacker to happen — split-horizon DNS or a
wildcard-redirecting resolver produces it by accident.

What the guard does not address is the larger risk, because that one needs no
private address at all. An attacker on the path to the plaintext geo list can
inject a mirror they control on an ordinary public address, and it will be
ranked like any other. apt verifies archive signatures, so such a mirror cannot
inject packages into your system; what it can do is serve a genuine but older
signed archive indefinitely, holding back security updates. aptmir's freshness
column is what makes that visible — `BEHIND` is worth reading, not just
`BANDWIDTH`.

Text that a mirror chose is stripped of control characters before it is printed.
The `STATUS` column quotes index paths from the mirror's own release file when
verification fails, and an escape sequence there would otherwise be able to
rewrite the table around it.

Earlier versions shipped an `-apply` flag that rewrote `/etc/apt` to the
winning mirror. It was removed: ranking depends on live latency and
throughput samples, so two runs minutes apart can legitimately disagree, and
wiring a nondeterministic measurement to a persistent change of system files
is not a trade worth making. Treat the table as a recommendation and make the
change deliberately.

## Notes on releases

- The codename comes from `/etc/os-release`, and from nowhere else. It is
  authoritative for the running system and present on every supported release,
  so no release-specific handling is needed as new versions land. If the file is
  missing or carries no `VERSION_CODENAME`, the run stops and asks for
  `-codename` rather than guessing.
- Earlier versions fell back to scanning `/etc/apt` and then to `lsb_release`.
  Both were dropped. Deciding whether a source line belongs to the Ubuntu
  archive meant guessing from its host and path, and the guess was wrong in both
  directions: any third-party repository published under a `/ubuntu` path was
  accepted, as was any host whose name merely ended in the right letters. A
  wrong codename is not obviously wrong — it ranks mirrors for a release you are
  not running — so a second source of truth that can disagree with the first is
  worse than no second source at all.
- If the release is end-of-life, the tool detects that the archive no longer
  carries it and points you at `old-releases.ubuntu.com` rather than ranking
  mirrors that cannot serve you.
- On arm64/ppc64el/s390x/riscv64 it uses `ports.ubuntu.com`. Since
  `mirrors.ubuntu.com` only indexes the main archive, Launchpad is the only
  source of a wider candidate list for those architectures.

## Flags

```text
-codename string     release codename (default: autodetect)
-arch string         dpkg architecture (default: autodetect)
-country string      restrict to these countries, comma-separated two-letter codes, e.g. DE,PL
-scheme string       keep only candidates already published under this scheme: http, https, or any (default any)
-limit int           cap the candidate list at this many mirrors, 0 for no cap (default 0)
-screen-top int      check freshness and sync locks on this many closest candidates (default 50)
-no-cache            force probes past any CDN cache, measuring the cold first-fetch path
-include-csp         also consider the cloud provider archives (AWS regional, Azure)
-max-age duration    reject mirrors staler than this (default 24h)
-concurrency int     parallel probes (default 30)
-timeout duration    base timeout for a complete HTTP request (default 10s)
-probe-top int       measure bandwidth on this many best candidates, 0 for all (default 10)
-probe-bytes int     bytes timed per bandwidth probe, after a fixed warm-up is discarded (default 6 MiB)
-probe-time duration max duration per bandwidth probe (default 4s)
-no-bandwidth        skip throughput probes, rank on latency
-json                emit JSON instead of a table
-d, -debug           trace every request, result and measurement on stderr
-v, -version         print version information and exit
```

## Tests

```sh
go test -race ./...
```

Everything runs against in-process `httptest` servers — a synthetic Ubuntu
archive with a real `SHA256` stanza, plus purpose-built servers for the hostile
cases — so the suite needs no network and touches no real mirror.

| Area | What is covered |
| --- | --- |
| Release detection | suite parsing, `Date:` field parsing, codename detection inputs |
| Sync markers | absent, fresh, and stale markers; a pool that flaps per request and one that flaps per connection, which is how a real load-balanced host behaves |
| Index verification | a good mirror, a corrupt index, a mirror publishing only `Release`, an index declaring an implausible size, a declared index the mirror does not serve, and a mirror that streams a body forever |
| Probing | phase 3 runs strictly sequentially, three probes reach each mirror, the warm-up is excluded from the timed window, a body ending exactly at the warm-up boundary, a transfer that breaks inside the measured window, a window cut short just past the warm-up and one cut short only slightly, the low-confidence fallback, and stale candidates not consuming `-probe-top` slots |
| Caching | cache-busting by default and a cacheable target under `-no-cache`'s inverse, CDN detection across Cloudflare, Fastly, Varnish and CloudFront headers, and the bandwidth phase priming before it times |
| Candidate discovery | architecture filtering of ports versus main-archive URLs, `-scheme` filtering, `-country` list parsing, Launchpad link extraction against real URL shapes, the ISO country table, and the cloud provider archives including their exclusion on ports |
| Rotations | `archive.ubuntu.com` and its relatives recognised as pools, a single-server mirror serving from a path containing `archive.ubuntu.com` not mistaken for one |
| Ranking | clean mirrors above demoted ones |
| Staleness | the freshest evidence winning over a lagging pool backend, and a forged future date disregarded |
| Flags | `-concurrency`, `-probe-bytes`, `-probe-time`, `-timeout`, `-probe-top`, `-screen-top`, `-limit`, `-scheme` and `-country` validation |
| Errors | transport failures rendered short: TLS handshake timeout, connection refused, DNS failure, deadline |
| Output | table column alignment including low-confidence, error, pool and CDN rows; the summary block's percentiles, per-row sample counts and blank-line separation |
| Debug tracing | silent until enabled, logfmt shape including quoting, fifty concurrent writers without an interleaved line, progress counts suppressed under `-d`, and every line above the results structured |
| Version | `-v` and `-version`, and each build field individually present in the version string |
