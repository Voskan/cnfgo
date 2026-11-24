// Package testschema declares types that exercise every representation
// supported by cmd/cnfgo. Its generated code is checked in and verified by
// the generator's golden test.
package testschema

import (
	"errors"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo -type=Config,Tree -compact=Endpoint -codec=github.com/Voskan/cnfgo/internal/testschema.Money=github.com/Voskan/cnfgo/internal/testschema.MoneyCodec -codec=github.com/Voskan/cnfgo/internal/testschema.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]

// Kind is a named scalar.
type Kind uint8

// Label is a named string.
type Label string

// Config is the main root type.
type Config struct {
	Name     string
	Version  int64
	Enabled  bool
	Ratio    float64
	Weight   float32
	Small    int8
	Port     uint16
	Count    uint
	Kind     Kind
	Label    Label
	Cplx     complex128
	Blob     []byte
	Created  time.Time
	Addr     netip.Addr
	Prefix   netip.Prefix
	Endpoint netip.AddrPort
	Opt      *int64
	OptStr   *string
	OptTime  *time.Time
	OptBool  *bool
	Matrix   [4]int32
	Flags    [3]bool
	Tags     []string
	Ints     []int64
	Nested   Meta
	Owner    *User
	Users    []*User
	Metas    []Meta
	Labels   map[string]string
	Scores   map[string]float64 `cnf:",hash"`
	ByID     map[int64]*User    `cnf:",hash"`
	Weights  map[float64]string
	Toggles  map[bool]int32
	ByAddr   map[netip.Addr]string
	ByPrefix map[netip.Prefix]int32
	ByHost   map[HostKey]int64 `cnf:",hash"`
	Groups   []*Group
	Opts     []*int64
	Shape    Shape
	Shapes   []Shape
	Pairs    [2]Meta
	Grid     [][]int16
	Price    Money
	Limit    Quantity
	MaybeFee *Money
	Ends     []Endpoint
	Skip     string `cnf:"-"`
	_        int
}

// Meta is stored by value.
type Meta struct {
	Key   string
	Value string
	Count uint32
}

// User forms DAGs (and cycles through Friends).
type User struct {
	ID      int64
	Name    string
	Manager *User
	Friends []*User
	Roles   map[string][]string
}

// Tree has a parent back-pointer, so it can only be frozen with
// FreezeCyclicTree.
type Tree struct {
	Value    int
	Children []*Tree
	Parent   *Tree
}

// HostKey is a flat comparable struct used as a map key.
type HostKey struct {
	Addr netip.Addr
	Port uint16
	Name string
}

// Group and Policy form a mutually recursive strongly connected component.
type Group struct {
	Name     string
	Policies []*Policy
}

// Policy references back to Group, exercising cross-table acyclicity checks.
type Policy struct {
	ID     string
	Groups []*Group
}

// Shape is a closed interface.
//
//cnf:implementations=Circle,*Rect
type Shape interface {
	Area() float64
}

// Circle implements Shape by value.
type Circle struct{ R float64 }

// Area implements Shape.
func (c Circle) Area() float64 { return math.Pi * c.R * c.R }

// Rect implements Shape by pointer.
type Rect struct{ W, H float64 }

// Area implements Shape.
func (r *Rect) Area() float64 { return r.W * r.H }

// Endpoint has a Compact form.
type Endpoint struct {
	Name    string
	Port    uint16
	Weight  float64
	Up      bool
	Addr    netip.Addr
	Prefix  netip.Prefix
	AP      netip.AddrPort
	Seen    time.Time
	Opt     *int32
	OptName *string
	Arr     [2]uint16
	Blob    []byte
	Tags    []string
	Meta    Meta
	Fee     Money
}

// Money is an opaque type with a registered codec.
type Money struct {
	units    int64
	currency string
}

// NewMoney returns an amount in minor units.
func NewMoney(units int64, currency string) Money { return Money{units, currency} }

// Units returns the amount in minor units.
func (m Money) Units() int64 { return m.units }

// Currency returns the currency code.
func (m Money) Currency() string { return m.currency }

// MoneyCodec encodes Money as "<units> <currency>".
type MoneyCodec struct{}

// Size implements cnfrt.Codec.
func (MoneyCodec) Size(m Money) int { return len(MoneyCodec{}.Append(nil, m)) }

// Append implements cnfrt.Codec.
func (MoneyCodec) Append(dst []byte, m Money) []byte {
	dst = strconv.AppendInt(dst, m.units, 10)
	dst = append(dst, ' ')
	return append(dst, m.currency...)
}

// Decode implements cnfrt.Codec.
func (MoneyCodec) Decode(src string) (Money, error) {
	u, c, ok := strings.Cut(src, " ")
	if !ok {
		return Money{}, errors.New("testschema: malformed money")
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil {
		return Money{}, err
	}
	return Money{n, strings.Clone(c)}, nil
}

// Quantity mimics the method set of resource.Quantity used by the k8s codec.
type Quantity struct {
	milli int64
}

// MilliQuantity returns a quantity of n thousandths.
func MilliQuantity(n int64) Quantity { return Quantity{n} }

// MilliValue returns the quantity in thousandths.
func (q *Quantity) MilliValue() int64 { return q.milli }

// String returns the canonical form, for example "1500m".
func (q *Quantity) String() string { return strconv.FormatInt(q.milli, 10) + "m" }

// UnmarshalJSON parses a quoted canonical form.
func (q *Quantity) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
	if err != nil {
		return err
	}
	q.milli = n
	return nil
}
