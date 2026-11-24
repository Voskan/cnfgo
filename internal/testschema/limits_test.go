package testschema

import (
	"errors"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

// chain returns a Tree list of n nodes linked through Children[0].
func chain(n int) *Tree {
	root := &Tree{Value: 0}
	cur := root
	for i := 1; i < n; i++ {
		next := &Tree{Value: i}
		cur.Children = []*Tree{next}
		cur = next
	}
	return root
}

func TestGraphDepthLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds deep graphs")
	}
	const n = 40_000
	deep := chain(n)
	if _, err := FreezeTree(deep); !errors.Is(err, cnfrt.ErrCapacityExceeded) {
		t.Fatalf("FreezeTree(%d-deep list) = %v, want ErrCapacityExceeded", n, err)
	}
	s, err := FreezeTree(deep, cnfrt.WithMaxDepth(cnfrt.MaxGraphDepth))
	if err != nil {
		t.Fatalf("FreezeTree(%d-deep list, MaxGraphDepth): %v", n, err)
	}
	depth := 0
	for v := s.RootTree(); v.IsValid(); depth++ {
		if v.Value() != depth {
			t.Fatalf("node %d has Value %d", depth, v.Value())
		}
		if v.Children().Len() == 0 {
			break
		}
		v = v.Children().At(0)
	}
	if depth != n-1 {
		t.Fatalf("walked %d edges, want %d", depth, n-1)
	}

	// Larger limits are capped at MaxGraphDepth.
	if _, err := FreezeTree(chain(cnfrt.MaxGraphDepth+8), cnfrt.WithMaxDepth(1<<30)); !errors.Is(err, cnfrt.ErrCapacityExceeded) {
		t.Fatalf("FreezeTree beyond MaxGraphDepth = %v, want ErrCapacityExceeded", err)
	}
}

func TestPackTreeAndCyclic(t *testing.T) {
	ep := &Endpoint{Name: "edge", Port: 443, Tags: []string{"a", "b", "a"}, Meta: Meta{Key: "k"}}
	plain, err := PackEndpoint(ep)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := PackTreeEndpoint(ep)
	if err != nil {
		t.Fatal(err)
	}
	cyclic, err := PackCyclicEndpoint(ep)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]CompactEndpoint{"Pack": plain, "PackTree": tree, "PackCyclic": cyclic} {
		if c.Name() != "edge" || c.Port() != 443 || c.View().Tags().Len() != 3 || c.View().Meta().Key() != "k" {
			t.Errorf("%s: decoded value mismatch", name)
		}
		if _, err := ParseCompactEndpoint(c.Raw(), cnfrt.VerifyUntrusted); err != nil {
			t.Errorf("%s: ParseCompactEndpoint: %v", name, err)
		}
	}
	if plain.Raw()[0]&cnfrt.SlimFlagCyclic != 0 || tree.Raw()[0]&cnfrt.SlimFlagCyclic != 0 {
		t.Error("acyclic Pack set SlimFlagCyclic")
	}
	if cyclic.Raw()[0]&cnfrt.SlimFlagCyclic == 0 {
		t.Error("PackCyclic did not set SlimFlagCyclic")
	}
	d, err := cyclic.Detach()
	if err != nil {
		t.Fatal(err)
	}
	if d.Raw()[0]&cnfrt.SlimFlagCyclic == 0 {
		t.Error("Detach dropped the cyclic mode")
	}
}
