package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// staleAfter is how old a sync lock must be before it is treated as leaked
// rather than as a sync that is genuinely running.
const staleAfter = time.Hour

// maxIndexBytes is the largest index file this tool will read while verifying a
// mirror. Sizes come from the mirror's own InRelease, so an unbounded read is an
// unbounded read of attacker-controlled length: a mirror that shows a marker,
// declares Size 1000000000000 and then streams forever would otherwise stall
// phase 1 until the process is killed. The spec's verified set (Decision 4) is
// 12 files and 30.9 MB for resolute/amd64, of which the largest single file —
// universe/binary-amd64/Packages.gz — is about 19 MB. 64 MiB is over three
// times that largest legitimate index and over twice the whole verified set, so
// it leaves years of archive growth in hand while still being a bound: anything
// declaring more than this is not an apt index and is rejected without being
// fetched at all.
const maxIndexBytes = 64 << 20

// markerState describes the result of checking for the
// Archive-Update-in-Progress lock on a mirror.
type markerState int

const (
	markerNone    markerState = iota // No lock file present.
	markerFresh                      // Lock present and recent: a sync is running now.
	markerStale                      // Lock present but old, or its age is unknown.
	markerUnknown                    // The mirror's root could not be checked.
)

// String renders the state for the status column and JSON output.
func (s markerState) String() string {
	switch s {
	case markerFresh:
		return "syncing"
	case markerStale:
		return "stale-lock"
	case markerUnknown:
		return "marker-unknown"
	default:
		return ""
	}
}

// markerInfo is what detectMarker found in the archive root.
type markerInfo struct {
	State    markerState
	Name     string
	Modified time.Time
	// Unconfirmed records that a marker was seen on the first request but not
	// on the second, which is how a pooled host behaves. The mirror is treated
	// as clean, but the two cases are worth telling apart in the log: without
	// this they are both an empty markerInfo and the confirmation step looks
	// like it never does anything.
	Unconfirmed bool
}

// markerAge renders the marker's own Last-Modified for the verbose log, which
// is what tells a leaked lock from a sync that started a minute ago.
func markerAge(info markerInfo) string {
	if info.Modified.IsZero() {
		return "unknown (no usable Last-Modified)"
	}
	return info.Modified.UTC().Format(time.RFC1123)
}

var markerHrefRe = regexp.MustCompile(`href="(Archive-Update-in-Progress-[^"]+)"`)

// detectMarker reports whether a mirror is mid-sync. A marker is confirmed with
// a second request, because pooled hosts such as archive.ubuntu.com answer from
// several backends and only some of them carry the file. The age comes from the
// marker's own Last-Modified header rather than from the directory listing,
// which is autoindex HTML and varies by server.
func detectMarker(ctx context.Context, client *http.Client, base string) (markerInfo, error) {
	name, err := markerName(ctx, client, base)
	if err != nil {
		return markerInfo{}, err
	}
	if name == "" {
		return markerInfo{}, nil
	}

	// The confirmation is only worth making on a connection the first request
	// did not use. A DNS or load-balancer pool routes per connection, not per
	// request: markerName drains the small root listing, so its connection goes
	// straight back to the idle pool and a plain second GET would reuse it,
	// reach the same backend, and confirm itself. Cloning the transport gives
	// the confirmation an empty pool and therefore a new connection.
	confirmClient, closeIdle := freshConnClient(client)
	defer closeIdle()

	debugLog.Debug("marker seen", "mirror", base, "marker", name, "confirming", true)
	confirm, err := markerName(ctx, confirmClient, base)
	ageClient := confirmClient
	if err != nil {
		// A confirmation that never answered is not an answer that the marker
		// is gone. Keep the sighting and try its Last-Modified through the
		// original client, whose transport may reuse the connection that saw
		// the marker. If that also fails, markerStale remains the conservative
		// state for a lock whose age could not be read.
		debugLog.Debug("marker unconfirmable", "mirror", base, "marker", name, "err", shortErr(err))
		ageClient = client
	} else if confirm == "" {
		return markerInfo{Name: name, Unconfirmed: true}, nil
	}

	info := markerInfo{State: markerStale, Name: name}
	// After a successful confirmation the age comes from that backend. If the
	// confirmation failed, the original client's idle connection is the best
	// chance of reaching the backend that supplied the initial sighting.
	mod, err := markerModified(ctx, ageClient, base+name)
	if err == nil && !mod.IsZero() {
		info.Modified = mod
		if time.Since(mod) < staleAfter {
			info.State = markerFresh
		}
	}
	return info, nil
}

// freshConnClient returns a client that shares the original's settings but not
// its connection pool, so a request made through it cannot reuse a connection
// the caller already opened, plus a function that closes what it did open. A
// transport this code did not create cannot be cloned, in which case the caller
// gets its own client back and the confirmation degrades to what it was before.
func freshConnClient(client *http.Client) (*http.Client, func()) {
	rt := client.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	tr, ok := rt.(*http.Transport)
	if !ok {
		return client, func() {}
	}
	fresh := tr.Clone()
	clone := *client
	clone.Transport = fresh
	return &clone, fresh.CloseIdleConnections
}

// markerName returns the marker filename listed in the archive root, or "".
func markerName(ctx context.Context, client *http.Client, base string) (string, error) {
	body, err := get(ctx, client, base)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 512<<10))
	if err != nil {
		return "", err
	}
	if m := markerHrefRe.FindSubmatch(data); m != nil {
		return string(m[1]), nil
	}
	return "", nil
}

// markerModified reads the marker's Last-Modified header.
func markerModified(ctx context.Context, client *http.Client, u string) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	// A HEAD carries no body, but draining before closing keeps the connection
	// reusable and keeps the pattern correct if this ever becomes a GET.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	return http.ParseTime(resp.Header.Get("Last-Modified"))
}

// indexRef is one file listed in the SHA256 stanza of InRelease.
type indexRef struct {
	Path   string
	Size   int64
	SHA256 string
}

// parseInReleaseIndexes reads the SHA256 stanza. Lines in the stanza start with
// a space and carry hash, size and path; the stanza ends at the next field. The
// stanza has the same shape in InRelease and in the detached Release file, so
// this parses either.
func parseInReleaseIndexes(data []byte) []indexRef {
	var out []indexRef
	inStanza := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "SHA256:") {
			inStanza = true
			continue
		}
		if !inStanza {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, indexRef{Path: fields[2], Size: size, SHA256: fields[0]})
	}
	return out
}

// verifiableIndexes selects the indexes apt actually reads on this machine: the
// binary package lists for the running architecture and the English translation
// files. Verifying every entry is infeasible, being dominated by uncompressed
// Contents-* files of roughly 880 MB each.
func verifiableIndexes(refs []indexRef, arch string) []indexRef {
	suffix := "binary-" + arch + "/Packages.gz"
	var out []indexRef
	for _, r := range refs {
		if strings.HasSuffix(r.Path, suffix) || strings.HasSuffix(r.Path, "i18n/Translation-en.gz") {
			out = append(out, r)
		}
	}
	return out
}

// verifyIndexes checks each selected index against the hash the mirror's own
// release file declares.
// A mismatch means the mirror's dists tree is internally inconsistent, which is
// the failure a stale sync lock warns about and the freshness check cannot see.
func verifyIndexes(ctx context.Context, client *http.Client, base, codename, arch string) error {
	dists := base + "dists/" + codename + "/"
	data, err := fetchReleaseFile(ctx, client, dists)
	if err != nil {
		return err
	}

	refs := verifiableIndexes(parseInReleaseIndexes(data), arch)
	debugLog.Debug("verify start", "mirror", base, "indexes", len(refs), "arch", arch)
	if len(refs) == 0 {
		return errors.New("release file lists no index for this architecture")
	}
	for _, ref := range refs {
		// The declared size decides how much this tool is willing to read, so
		// check it before opening the connection: an implausible size is itself
		// grounds to reject the mirror, not something to read far enough to
		// discover.
		if ref.Size < 0 || ref.Size > maxIndexBytes {
			return fmt.Errorf("%s: declared size %d is outside the plausible range for an index (max %d)",
				ref.Path, ref.Size, maxIndexBytes)
		}
		if err := verifyOneIndex(ctx, client, dists+ref.Path, ref); err != nil {
			return err
		}
	}
	return nil
}

// fetchReleaseFile reads the signed index manifest, preferring InRelease and
// falling back to the detached Release file exactly as fetchReleaseDate does: a
// healthy mirror that publishes only Release must not be reported unreachable
// merely because it also carries a sync marker.
func fetchReleaseFile(ctx context.Context, client *http.Client, dists string) ([]byte, error) {
	var lastErr error
	for _, name := range []string{"InRelease", "Release"} {
		body, err := get(ctx, client, dists+name)
		if err != nil {
			lastErr = fmt.Errorf("fetch %s: %w", name, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(body, 4<<20))
		_ = body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read %s: %w", name, err)
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// verifyOneIndex streams a single index and compares its digest.
func verifyOneIndex(ctx context.Context, client *http.Client, u string, ref indexRef) error {
	body, err := get(ctx, client, u)
	if err != nil {
		return fmt.Errorf("%s: %w", ref.Path, err)
	}
	defer func() { _ = body.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(body, ref.Size+1))
	if err != nil {
		return fmt.Errorf("%s: %w", ref.Path, err)
	}
	if n != ref.Size {
		return fmt.Errorf("%s: got %d bytes, release file declares %d", ref.Path, n, ref.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != ref.SHA256 {
		return fmt.Errorf("%s: sha256 %s does not match release file %s", ref.Path, sum, ref.SHA256)
	}
	debugLog.Debug("verify ok", "url", u, "bytes", n)
	return nil
}
