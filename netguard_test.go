package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// newTestClient builds the client the tests use. It shares every transport
// setting with newClient — the timeout contract in particular — but carries no
// address guard, because httptest listens on loopback and the production guard
// exists precisely to refuse that.
func newTestClient(timeout time.Duration) *http.Client {
	return newClientWithControl(timeout, nil)
}

func TestPublicOnlyAddrRejectsNonPublicDestinations(t *testing.T) {
	rejected := []string{
		"127.0.0.1",        // loopback
		"::1",              // loopback, v6
		"10.0.0.1",         // RFC 1918
		"172.16.0.1",       // RFC 1918
		"192.168.1.1",      // RFC 1918
		"fd00::1",          // unique local, the v6 equivalent
		"169.254.169.254",  // link-local, the cloud metadata endpoint
		"fe80::1",          // link-local, v6
		"0.0.0.0",          // unspecified
		"::",               // unspecified, v6
		"224.0.0.1",        // multicast
		"ff02::1",          // multicast, v6
		"100.64.0.1",       // carrier-grade NAT
		"::ffff:127.0.0.1", // loopback smuggled through a v4-mapped v6 address
	}
	for _, s := range rejected {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if err := publicOnlyAddr(addr); err == nil {
			t.Errorf("publicOnlyAddr(%s) = nil, want a refusal", s)
		}
	}

	accepted := []string{"1.1.1.1", "91.189.91.38", "2001:67c:1360:8001::24"}
	for _, s := range accepted {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if err := publicOnlyAddr(addr); err != nil {
			t.Errorf("publicOnlyAddr(%s) = %v, want nil", s, err)
		}
	}
}

// The guard runs after resolution, so a hostname is judged by the address it
// actually resolves to rather than by its spelling. That is what covers a
// mirror whose DNS points inside the network, and what makes a rebind between
// check and connect impossible: there is no separate check to race.
func TestNewClientRefusesAHostResolvingToLoopback(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := get(t.Context(), newClient(time.Second), srv.URL+"/")
	if err == nil {
		t.Fatal("request to a loopback mirror succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "non-public address") {
		t.Errorf("error = %v, want it to name the non-public address", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("mirror served %d requests, want 0: the guard must refuse before connecting", n)
	}
}

// A candidate that passes the guard can still redirect somewhere that does not.
// Every hop opens its own connection, so the guard sees each one; this pins
// that rather than leaving it to be inferred.
func TestGuardAppliesToEveryRedirectHop(t *testing.T) {
	var hits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/", http.StatusFound)
	}))
	defer public.Close()

	// Stand in for the real policy: the first hop is permitted, the second is
	// not. Both are on loopback, which no real policy could tell apart.
	blocked := strings.TrimPrefix(internal.URL, "http://")
	errBlocked := errors.New("refused by the test policy")
	control := func(_, address string, _ syscall.RawConn) error {
		if address == blocked {
			return errBlocked
		}
		return nil
	}

	_, err := get(t.Context(), newClientWithControl(time.Second, control), public.URL+"/")
	if err == nil {
		t.Fatal("redirect to a refused address succeeded, want a refusal")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("redirect target served %d requests, want 0", n)
	}
}
