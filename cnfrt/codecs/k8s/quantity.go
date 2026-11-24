// Package k8s provides cnfrt codecs for Kubernetes API types without
// depending on the Kubernetes modules.
//
// cmd/cnfgo applies Quantity automatically to fields of type
// k8s.io/apimachinery/pkg/api/resource.Quantity, as if invoked with
//
//	-codec 'k8s.io/apimachinery/pkg/api/resource.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]'
package k8s

import (
	"fmt"
	"strconv"
)

// QuantityPtr is the method set of *resource.Quantity used by the codec.
type QuantityPtr[Q any] interface {
	*Q
	String() string
	UnmarshalJSON([]byte) error
}

// Quantity is a cnfrt.Codec for resource.Quantity-like types Q. A value is
// stored as its canonical string form (for example "1500m" or "2Gi"), which
// round-trips exactly through parsing.
type Quantity[Q any, P QuantityPtr[Q]] struct{}

// Size returns the encoded size of v.
func (Quantity[Q, P]) Size(v Q) int { return len(P(&v).String()) }

// Append appends the canonical string form of v to dst.
func (Quantity[Q, P]) Append(dst []byte, v Q) []byte { return append(dst, P(&v).String()...) }

// Decode parses a quantity encoded by Append.
func (Quantity[Q, P]) Decode(src string) (Q, error) {
	var q Q
	if err := P(&q).UnmarshalJSON([]byte(strconv.Quote(src))); err != nil {
		return q, fmt.Errorf("k8s: decoding quantity %q: %w", src, err)
	}
	return q, nil
}
