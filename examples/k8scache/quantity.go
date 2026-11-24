package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Quantity is a fixed-point resource amount with the method set of
// resource.Quantity that the k8s codec relies on. It keeps amounts in
// thousandths and remembers whether binary (Ki, Mi, Gi) suffixes are used.
type Quantity struct {
	milli  int64
	binary bool
}

// MilliCPU returns a CPU quantity of n thousandths of a core.
func MilliCPU(n int64) Quantity { return Quantity{milli: n} }

// Bytes returns a memory quantity of n bytes written with binary suffixes.
func Bytes(n int64) Quantity { return Quantity{milli: n * 1000, binary: true} }

// MilliValue returns the amount in thousandths.
func (q *Quantity) MilliValue() int64 { return q.milli }

// Value returns the amount rounded up to an integer.
func (q *Quantity) Value() int64 { return (q.milli + 999) / 1000 }

var binarySuffixes = []struct {
	s string
	n int64
}{{"Ei", 1 << 60}, {"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}}

// String returns the canonical form, for example "250m", "2" or "512Mi".
func (q *Quantity) String() string {
	if q.milli%1000 != 0 {
		return strconv.FormatInt(q.milli, 10) + "m"
	}
	v := q.milli / 1000
	if q.binary && v != 0 {
		for _, s := range binarySuffixes {
			if v%s.n == 0 {
				return strconv.FormatInt(v/s.n, 10) + s.s
			}
		}
	}
	return strconv.FormatInt(v, 10)
}

// UnmarshalJSON parses a quoted canonical form.
func (q *Quantity) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("quantity: %w", err)
	}
	if n, ok := strings.CutSuffix(s, "m"); ok {
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return fmt.Errorf("quantity %q: %w", s, err)
		}
		*q = Quantity{milli: v}
		return nil
	}
	for _, suf := range binarySuffixes {
		if n, ok := strings.CutSuffix(s, suf.s); ok {
			v, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return fmt.Errorf("quantity %q: %w", s, err)
			}
			*q = Quantity{milli: v * suf.n * 1000, binary: true}
			return nil
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("quantity %q: %w", s, err)
	}
	*q = Quantity{milli: v * 1000}
	return nil
}
