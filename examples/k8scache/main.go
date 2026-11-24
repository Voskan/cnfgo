package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func main() {
	pods := flag.Int("pods", 100_000, "number of cached pods")
	flag.Parse()
	if err := run(os.Stdout, *pods); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// makePod returns the i-th pod of a deterministic synthetic cluster.
func makePod(i int) *Pod {
	ns := fmt.Sprintf("team-%d", i%50)
	app := fmt.Sprintf("svc-%d", i%400)
	prio := int32(i % 3 * 1000)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
	return &Pod{
		Namespace: ns,
		Name:      fmt.Sprintf("%s-%08x", app, i),
		UID:       fmt.Sprintf("6f1c%04x-1d2e-4f3a-9b8c-%012x", i%0xffff, i),
		Labels: map[string]string{
			"app":                       app,
			"app.kubernetes.io/part-of": ns,
			"pod-template-hash":         fmt.Sprintf("%08x", i%400*7919),
		},
		Annotations: map[string]string{"prometheus.io/scrape": "true", "prometheus.io/port": "9090"},
		NodeName:    fmt.Sprintf("node-%d.cluster.internal", i%1000),
		Phase:       Running,
		PodIP:       netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}),
		StartTime:   start,
		Priority:    &prio,
		Containers: []Container{
			{
				Name:  "app",
				Image: fmt.Sprintf("registry.example.com/%s/%s:v1.%d.0", ns, app, i%20),
				Args:  []string{"--port=8080", "--log-level=info"},
				Ports: []ContainerPort{{Name: "http", Port: 8080, Protocol: "TCP"}, {Name: "metrics", Port: 9090, Protocol: "TCP"}},
				Resources: Resources{
					CPURequest: MilliCPU(250), CPULimit: MilliCPU(1000),
					MemoryRequest: Bytes(256 << 20), MemoryLimit: Bytes(1 << 30),
				},
			},
			{
				Name:  "proxy",
				Image: "registry.example.com/mesh/proxy:v2.4.1",
				Ports: []ContainerPort{{Name: "admin", Port: 15000, Protocol: "TCP"}},
				Resources: Resources{
					CPURequest: MilliCPU(100), CPULimit: MilliCPU(500),
					MemoryRequest: Bytes(64 << 20), MemoryLimit: Bytes(256 << 20),
				},
			},
		},
		Conditions: []Condition{
			{Type: "Ready", Status: true, LastTransitionTime: start.Add(5 * time.Second)},
			{Type: "ContainersReady", Status: true, LastTransitionTime: start.Add(5 * time.Second)},
		},
	}
}

func key(p *Pod) string { return p.Namespace + "/" + p.Name }

// footprint describes the heap cost of keeping one cache alive.
type footprint struct {
	heap   uint64        // live heap bytes
	scan   uint64        // bytes the collector must scan
	gcTime time.Duration // duration of a full collection
}

func measure(keep func()) footprint {
	base := heapNow()
	keep()
	f := heapNow()
	// The collector may release unrelated memory between the two samples,
	// so a difference that would underflow is reported as zero.
	f.heap = subFloor(f.heap, base.heap)
	f.scan = subFloor(f.scan, base.scan)
	return f
}

// subFloor returns a-b, or 0 if b > a.
func subFloor(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func heapNow() footprint {
	runtime.GC()
	start := time.Now()
	runtime.GC()
	d := time.Since(start)
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/scan/heap:bytes"}}
	metrics.Read(s)
	var f footprint
	if s[0].Value.Kind() == metrics.KindUint64 {
		f.heap = s[0].Value.Uint64()
	}
	if s[1].Value.Kind() == metrics.KindUint64 {
		f.scan = s[1].Value.Uint64()
	}
	f.gcTime = d
	return f
}

func (f footprint) String() string {
	return fmt.Sprintf("heap %7.1f MiB, scannable %7.1f MiB, full GC %8v", float64(f.heap)/(1<<20), float64(f.scan)/(1<<20), f.gcTime.Round(10*time.Microsecond))
}

func run(w io.Writer, n int) error {
	var probe *Pod
	fmt.Fprintf(w, "caching %d pods (2 containers each, CPU and memory quantities)\n", n)

	// 1. Ordinary Go objects.
	var native map[string]*Pod
	f := measure(func() {
		native = make(map[string]*Pod, n)
		for i := range n {
			p := makePod(i)
			native[key(p)] = p
		}
	})
	probe = makePod(n / 2)
	if got := native[key(probe)]; got == nil || got.UID != probe.UID {
		return fmt.Errorf("native cache lost %s", key(probe))
	}
	fmt.Fprintf(w, "  map[string]*Pod            %v\n", f)
	native = nil

	// 2. A Go map of Compact values: one string header per pod.
	var compact map[string]CompactPod
	var packErr error
	f = measure(func() {
		compact = make(map[string]CompactPod, n)
		for i := range n {
			p := makePod(i)
			c, err := PackPod(p)
			if err != nil {
				packErr = err
				return
			}
			compact[key(p)] = c
		}
	})
	if packErr != nil {
		return packErr
	}
	c := compact[key(probe)]
	if c.UID() != probe.UID || c.PodIP() != probe.PodIP {
		return fmt.Errorf("compact cache returned %s, %v", c.UID(), c.PodIP())
	}
	fmt.Fprintf(w, "  map[string]CompactPod      %v\n", f)
	compact = nil

	// 3. A SlabMap: index and records contain no pointers at all.
	var slab *cnfrt.SlabMap[string, CompactPod]
	f = measure(func() {
		slab, packErr = cnfrt.NewSlabMap[string, CompactPod](nil)
		if packErr != nil {
			return
		}
		for i := range n {
			p := makePod(i)
			c, err := PackPod(p)
			if err == nil {
				err = slab.Put(key(p), c)
			}
			if err != nil {
				packErr = err
				return
			}
		}
	})
	if packErr != nil {
		return packErr
	}
	c, ok := slab.Get(key(probe))
	if !ok {
		return fmt.Errorf("slab cache lost %s", key(probe))
	}
	view := c.View()
	app := view.Containers().At(0)
	res := app.Resources()
	cpu, err := res.CPULimit()
	if err != nil {
		return err
	}
	mem, err := res.MemoryRequest()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  SlabMap[string, CompactPod] %v\n", f)
	st := slab.Stats()
	fmt.Fprintf(w, "slab map: %d pods in %d slabs, %.1f MiB live\n", st.Len, st.Slabs, float64(st.LiveBytes)/(1<<20))
	runtime.KeepAlive(slab)
	slab = nil

	// 4. CompactPod with a shared pointer-free Dict (seen-twice admission).
	var compactDict map[string]CompactPod
	var dict1 *cnfrt.Dict
	f = measure(func() {
		dict1 = cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
		compactDict = make(map[string]CompactPod, n)
		for i := range n {
			p := makePod(i)
			c, err := PackPod(p, cnfrt.WithDict(dict1))
			if err != nil {
				packErr = err
				return
			}
			compactDict[key(p)] = c
		}
	})
	if packErr != nil {
		dict1.Close()
		return packErr
	}
	c = compactDict[key(probe)]
	if c.UID() != probe.UID || c.PodIP() != probe.PodIP {
		dict1.Close()
		return fmt.Errorf("compact-dict cache returned %s, %v", c.UID(), c.PodIP())
	}
	fmt.Fprintf(w, "  map[string]CompactPod+Dict %v\n", f)
	dst1 := dict1.Stats()
	fmt.Fprintf(w, "  dict: %d shared strings, %.2f MiB strings, %.2f MiB total in %d chunk(s)\n",
		dst1.Entries, float64(dst1.Bytes)/(1<<20), float64(dst1.MemBytes)/(1<<20), dst1.Chunks)
	compactDict = nil
	dict1.Close()

	// 5. SlabMap + shared Dict: zero GC scan and maximum memory compression.
	var slabDict *cnfrt.SlabMap[string, CompactPod]
	var dict2 *cnfrt.Dict
	f = measure(func() {
		dict2 = cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
		slabDict, packErr = cnfrt.NewSlabMap[string, CompactPod](nil)
		if packErr != nil {
			return
		}
		for i := range n {
			p := makePod(i)
			c, err := PackPod(p, cnfrt.WithDict(dict2))
			if err == nil {
				err = slabDict.Put(key(p), c)
			}
			if err != nil {
				packErr = err
				return
			}
		}
	})
	if packErr != nil {
		dict2.Close()
		return packErr
	}
	defer dict2.Close()
	c, ok = slabDict.Get(key(probe))
	if !ok {
		return fmt.Errorf("slab-dict cache lost %s", key(probe))
	}
	view = c.View()
	app = view.Containers().At(0)
	res = app.Resources()
	cpu, err = res.CPULimit()
	if err != nil {
		return err
	}
	mem, err = res.MemoryRequest()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  SlabMap+Dict               %v\n", f)
	stDict := slabDict.Stats()
	fmt.Fprintf(w, "slab+dict map: %d pods in %d slabs, %.1f MiB live\n", stDict.Len, stDict.Slabs, float64(stDict.LiveBytes)/(1<<20))
	fmt.Fprintf(w, "%s: node %s, ip %s, image %s, cpu limit %s, memory request %s\n",
		key(probe), c.NodeName(), c.PodIP(), app.Image(), cpu.String(), mem.String())
	runtime.KeepAlive(slabDict)
	return nil
}
