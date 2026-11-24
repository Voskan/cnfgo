package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func main() {
	hosts := flag.Int("hosts", 2000, "number of routed hosts")
	dir := flag.String("dir", os.TempDir(), "directory for the .cnf file")
	flag.Parse()
	if err := run(os.Stdout, *hosts, *dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// buildBundle returns a deterministic bundle with the given number of hosts,
// each routed through a host-specific policy and the shared global policies.
func buildBundle(hosts int) *Bundle {
	global := []*Policy{
		{ID: "global-deny-list", Priority: 0, Enabled: true, Rules: []Rule{
			{ID: "deny-bad-ips", Match: []Matcher{SourceIn{List: "blocked"}}, Action: Action{Kind: Deny, Status: 403}},
		}},
		{ID: "global-admin", Priority: 1, Enabled: true, Rules: []Rule{
			{ID: "admin-internal-only", Match: []Matcher{PathPrefix{"/admin"}, &Not{Inner: SourceIn{List: "internal"}}}, Action: Action{Kind: Deny, Status: 404}},
		}},
		{ID: "disabled", Enabled: false, Rules: []Rule{{ID: "never", Action: Action{Kind: Deny, Status: 500}}}},
	}
	b := &Bundle{
		Revision: "rev-2026-11-22",
		Built:    time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC),
		Routes:   map[string]*Route{},
		IPLists: map[string][]netip.Prefix{
			"blocked":  {netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("2001:db8:bad::/48")},
			"internal": {netip.MustParsePrefix("10.0.0.0/8")},
		},
		Default:  Action{Kind: Allow, Status: 200},
		Policies: global,
	}
	for h := range hosts {
		host := fmt.Sprintf("app%d.example.com", h)
		p := &Policy{ID: "policy-" + host, Priority: 10, Enabled: true, Tags: []string{"tenant"}}
		for r := range 8 {
			p.Rules = append(p.Rules, Rule{
				ID: fmt.Sprintf("%s-r%d", host, r),
				Match: []Matcher{
					PathPrefix{fmt.Sprintf("/api/v%d/", r)},
					MethodIn{[]string{"POST", "PUT", "DELETE"}},
					HeaderEquals{"X-Tenant", fmt.Sprintf("t%d", h)},
				},
				Action:    Action{Kind: Log, Status: 202, Headers: map[string]string{"X-Rule": "r"}},
				RateLimit: &RateLimit{Requests: 100, Window: int64(time.Minute)},
			})
		}
		b.Policies = append(b.Policies, p)
		b.Routes[host] = &Route{Host: host, Policies: append([]*Policy{p}, global...)}
	}
	return b
}

func testRequests() []*Request {
	return []*Request{
		{Host: "app1.example.com", Method: "GET", Path: "/", Source: netip.MustParseAddr("192.0.2.1")},
		{Host: "app1.example.com", Method: "GET", Path: "/", Source: netip.MustParseAddr("198.51.100.7")},
		{Host: "app1.example.com", Method: "GET", Path: "/admin/users", Source: netip.MustParseAddr("192.0.2.1")},
		{Host: "app1.example.com", Method: "GET", Path: "/admin/users", Source: netip.MustParseAddr("10.1.2.3")},
		{Host: "app1.example.com", Method: "POST", Path: "/api/v3/items", Source: netip.MustParseAddr("192.0.2.1"), Headers: map[string]string{"X-Tenant": "t1"}},
		{Host: "app1.example.com", Method: "POST", Path: "/api/v3/items", Source: netip.MustParseAddr("192.0.2.1"), Headers: map[string]string{"X-Tenant": "t2"}},
		{Host: "unknown.example.com", Method: "GET", Path: "/admin", Source: netip.MustParseAddr("198.51.100.7")},
	}
}

func run(w io.Writer, hosts int, dir string) error {
	b := buildBundle(hosts)
	snap, err := FreezeBundle(b)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "bundle.cnf")
	if err := WriteBundleSnapshotAtomic(path, snap); err != nil {
		return err
	}
	defer os.Remove(path)
	st, _ := os.Stat(path)
	rules := 0
	for p := range snap.Root().Policies().All() {
		rules += p.Rules().Len()
	}
	fmt.Fprintf(w, "bundle %s: %d hosts, %d policies, %d rules, %.1f MiB .cnf\n",
		b.Revision, len(b.Routes), snap.LenPolicy(), rules, float64(st.Size())/(1<<20))

	// Cold start: map the file and verify only the header.
	start := time.Now()
	mapped, err := MapBundleFile(path, cnfrt.VerifyTrustedHeader)
	if err != nil {
		return err
	}
	cold := time.Since(start)
	defer mapped.Close()

	// Subsequent opens no longer pay for one-time process initialization.
	reopens := make([]time.Duration, 100)
	for i := range reopens {
		t0 := time.Now()
		s, err := MapBundleFile(path, cnfrt.VerifyTrustedHeader)
		if err != nil {
			return err
		}
		reopens[i] = time.Since(t0)
		s.Close()
	}
	slices.Sort(reopens)
	fmt.Fprintf(w, "cold start from .cnf (mmap + header verification): first %v, median of %d reopens %v\n",
		cold.Round(time.Microsecond), len(reopens), reopens[len(reopens)/2].Round(time.Microsecond))

	view := mapped.Root()
	for _, r := range testRequests() {
		native, frozen := b.Evaluate(r), EvaluateView(view, r)
		if native != frozen {
			return fmt.Errorf("decision mismatch for %+v: native %+v, frozen %+v", r, native, frozen)
		}
		fmt.Fprintf(w, "%-6s %-20s %-14s from %-13s -> kind=%d status=%d rule=%q\n", r.Method, r.Host, r.Path, r.Source, frozen.Kind, frozen.Status, frozen.Rule)
	}
	return nil
}
