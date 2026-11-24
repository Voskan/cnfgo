package k8s

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

// quantity has the method set of resource.Quantity used by the codec.
type quantity struct{ milli int64 }

func (q *quantity) String() string { return strconv.FormatInt(q.milli, 10) + "m" }

func (q *quantity) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	q.milli, err = strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
	return err
}

func TestQuantity(t *testing.T) {
	var c cnfrt.Codec[quantity] = Quantity[quantity, *quantity]{}
	for _, v := range []int64{0, 1500, -250, 1 << 50} {
		q := quantity{v}
		enc := c.Append([]byte("x"), q)
		if len(enc) != 1+c.Size(q) {
			t.Fatalf("Size %d, appended %d", c.Size(q), len(enc)-1)
		}
		got, err := c.Decode(string(enc[1:]))
		if err != nil || got != q {
			t.Fatalf("round trip of %d: %+v, %v", v, got, err)
		}
	}
	if _, err := c.Decode("not-a-quantity"); err == nil {
		t.Fatal("malformed quantity accepted")
	}
}
