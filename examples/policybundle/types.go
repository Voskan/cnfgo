// Command policybundle compiles a web application firewall policy bundle
// into a .cnf file and evaluates requests directly against the memory-mapped
// snapshot, which starts in microseconds regardless of the bundle size.
package main

import (
	"net/netip"
	"strings"
	"time"
)

//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo -type=Bundle

// Bundle is a compiled set of policies and the routes they apply to.
type Bundle struct {
	Revision string
	Built    time.Time
	Policies []*Policy
	Routes   map[string]*Route `cnf:",hash"`
	IPLists  map[string][]netip.Prefix
	Default  Action
}

// Route binds a host name to the policies evaluated for it, in order.
type Route struct {
	Host     string
	Policies []*Policy
}

// Policy is an ordered list of rules.
type Policy struct {
	ID       string
	Priority int32
	Enabled  bool
	Rules    []Rule
	Tags     []string
}

// Rule applies its action when all matchers match.
type Rule struct {
	ID        string
	Match     []Matcher
	Action    Action
	RateLimit *RateLimit
}

// Action is the outcome of a matching rule.
type Action struct {
	Kind     ActionKind
	Status   uint16
	Redirect string
	Headers  map[string]string
}

// ActionKind enumerates actions.
type ActionKind uint8

// Actions.
const (
	Allow ActionKind = iota
	Deny
	RedirectTo
	Log
)

// RateLimit limits matching requests per client.
type RateLimit struct {
	Requests int32
	Window   int64 // nanoseconds
}

// Matcher tests one property of a request.
//
//cnf:implementations=PathPrefix,HeaderEquals,SourceIn,MethodIn,*Not
type Matcher interface {
	Matches(r *Request, b *Bundle) bool
}

// PathPrefix matches request paths with a prefix.
type PathPrefix struct{ Prefix string }

// HeaderEquals matches a request header value.
type HeaderEquals struct{ Name, Value string }

// SourceIn matches client addresses in a named IP list.
type SourceIn struct{ List string }

// MethodIn matches one of the listed methods.
type MethodIn struct{ Methods []string }

// Not inverts a matcher.
type Not struct{ Inner Matcher }

// Matches implements Matcher.
func (m PathPrefix) Matches(r *Request, _ *Bundle) bool { return strings.HasPrefix(r.Path, m.Prefix) }

// Matches implements Matcher.
func (m HeaderEquals) Matches(r *Request, _ *Bundle) bool { return r.Headers[m.Name] == m.Value }

// Matches implements Matcher.
func (m SourceIn) Matches(r *Request, b *Bundle) bool {
	for _, p := range b.IPLists[m.List] {
		if p.Contains(r.Source) {
			return true
		}
	}
	return false
}

// Matches implements Matcher.
func (m MethodIn) Matches(r *Request, _ *Bundle) bool {
	for _, x := range m.Methods {
		if x == r.Method {
			return true
		}
	}
	return false
}

// Matches implements Matcher.
func (m *Not) Matches(r *Request, b *Bundle) bool { return !m.Inner.Matches(r, b) }

// Request is an incoming HTTP request.
type Request struct {
	Host    string
	Method  string
	Path    string
	Source  netip.Addr
	Headers map[string]string
}
