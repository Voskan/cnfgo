package main

// Decision is the result of evaluating a request.
type Decision struct {
	Kind   ActionKind
	Status uint16
	Rule   string
}

// Evaluate evaluates r against the native bundle.
func (b *Bundle) Evaluate(r *Request) Decision {
	if route := b.Routes[r.Host]; route != nil {
		for _, p := range route.Policies {
			if !p.Enabled {
				continue
			}
			for _, rule := range p.Rules {
				if matchAll(rule.Match, r, b) {
					return Decision{rule.Action.Kind, rule.Action.Status, rule.ID}
				}
			}
		}
	}
	return Decision{b.Default.Kind, b.Default.Status, ""}
}

func matchAll(ms []Matcher, r *Request, b *Bundle) bool {
	for _, m := range ms {
		if !m.Matches(r, b) {
			return false
		}
	}
	return true
}

// EvaluateView evaluates r against a frozen bundle without allocating.
func EvaluateView(b BundleView, r *Request) Decision {
	if route, ok := b.Routes().Get(r.Host); ok && route.IsValid() {
		for p := range route.Policies().All() {
			if !p.Enabled() {
				continue
			}
			rules := p.Rules()
			for i := range rules.Len() {
				rule := rules.At(i)
				if matchAllView(b, rule.Match(), r) {
					a := rule.Action()
					return Decision{a.Kind(), a.Status(), rule.ID()}
				}
			}
		}
	}
	d := b.Default()
	return Decision{d.Kind(), d.Status(), ""}
}

func matchAllView(b BundleView, ms MatcherSliceView, r *Request) bool {
	for m := range ms.All() {
		if !matchView(b, m, r) {
			return false
		}
	}
	return true
}

func matchView(b BundleView, m MatcherView, r *Request) bool {
	if p, ok := m.AsPathPrefix(); ok {
		return p.PrefixLen() <= len(r.Path) && p.PrefixEquals(r.Path[:p.PrefixLen()])
	}
	if h, ok := m.AsHeaderEquals(); ok {
		return h.ValueEquals(r.Headers[h.Name()])
	}
	if s, ok := m.AsSourceIn(); ok {
		list, _ := b.IPLists().Get(s.List())
		for p := range list.All() {
			if p.Contains(r.Source) {
				return true
			}
		}
		return false
	}
	if mi, ok := m.AsMethodIn(); ok {
		methods := mi.Methods()
		for i := range methods.Len() {
			if methods.EqualsAt(i, r.Method) {
				return true
			}
		}
		return false
	}
	if n, ok := m.AsNot(); ok && n.IsValid() {
		return !matchView(b, n.Inner(), r)
	}
	return false
}
