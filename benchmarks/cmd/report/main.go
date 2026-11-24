// Command report runs every benchmark scenario of package benchmarks in a
// fresh child process, so that heap measurements do not interfere, and
// prints the results as Markdown tables.
//
// Usage:
//
//	go run ./benchmarks/cmd/report [-n 100000,1000000] [-cold 100000]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Voskan/cnfgo/benchmarks"
)

func main() {
	sizes := flag.String("n", "100000,1000000", "comma-separated catalog sizes")
	cold := flag.Int("cold", 100000, "catalog size for the cold start table")
	highCard := flag.Int("highcard", 0, "optional catalog size for high-cardinality unique-string table")
	child := flag.String("child", "", "internal: run one measurement and print it as JSON")
	n := flag.Int("child-n", 0, "internal: catalog size of the child measurement")
	dir := flag.String("dir", "", "internal: directory for temporary files")
	flag.Parse()
	if *child != "" {
		if err := runChild(os.Stdout, *child, *n, *dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := report(os.Stdout, *sizes, *cold, *highCard); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runChild(w io.Writer, what string, n int, dir string) error {
	enc := json.NewEncoder(w)
	switch {
	case what == "freeze":
		r, err := benchmarks.MeasureFreeze(n)
		if err != nil {
			return err
		}
		return enc.Encode(r)
	case strings.HasPrefix(what, "cold:"):
		d, size, err := benchmarks.ColdStart(strings.TrimPrefix(what, "cold:"), dir, uint64(n/2))
		if err != nil {
			return err
		}
		return enc.Encode([2]int64{int64(d), size})
	case strings.HasPrefix(what, "highcard:"):
		r, err := benchmarks.RunHighCard(strings.TrimPrefix(what, "highcard:"), n, benchmarks.ChurnBytes, dir)
		if err != nil {
			return err
		}
		return enc.Encode(r)
	default:
		r, err := benchmarks.Run(what, n, benchmarks.ChurnBytes, dir)
		if err != nil {
			return err
		}
		return enc.Encode(r)
	}
}

// spawn runs one measurement in a child process and decodes its result.
func spawn(what string, n int, dir string, out any) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "-child", what, "-child-n", strconv.Itoa(n), "-dir", dir)
	cmd.Env = append(os.Environ(), "GOGC=100", "GOMEMLIMIT=off")
	cmd.Stderr = os.Stderr
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s (n=%d): %w", what, n, err)
	}
	return json.Unmarshal(data, out)
}

func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

func dur(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%d ns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1f µs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2f ms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2f s", d.Seconds())
}

func report(w io.Writer, sizes string, cold, highCard int) error {
	dir, err := os.MkdirTemp("", "cnfgo-bench")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fmt.Fprintf(w, "%s %s/%s, %d CPUs, %s\n\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), time.Now().UTC().Format(time.DateOnly))
	var ns []int
	for f := range strings.SplitSeq(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid size %q", f)
		}
		ns = append(ns, n)
	}
	for _, n := range ns {
		fmt.Fprintf(w, "### %d items\n\n", n)
		fmt.Fprintf(w, "| Scenario | Build | Heap | Off-heap | Scanned heap | Full GC | Point read | Churn %d GiB: wall | GC cycles | GC CPU |\n", benchmarks.ChurnBytes>>30)
		fmt.Fprintln(w, "|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|")
		for _, s := range benchmarks.Scenarios {
			var r benchmarks.Result
			if err := spawn(s, n, dir, &r); err != nil {
				return err
			}
			build := "-"
			if r.Build > 0 {
				build = dur(r.Build)
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s | %s | %d | %s |\n",
				s, build, mib(r.Heap), mib(r.OffHeap), mib(r.Scan), dur(r.FullGC), dur(r.Read),
				dur(r.ChurnWall), r.ChurnGCs, dur(r.ChurnGCCPU))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "### Freeze cost")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Items | Time | Allocated | Allocations | Peak heap above source | Snapshot size |")
	fmt.Fprintln(w, "|--:|--:|--:|--:|--:|--:|")
	for _, n := range ns {
		var f benchmarks.FreezeCost
		if err := spawn("freeze", n, dir, &f); err != nil {
			return err
		}
		fmt.Fprintf(w, "| %d | %s | %s | %d | %s | %s |\n", n, dur(f.Time), mib(f.Allocated), f.Allocs, mib(f.Peak), mib(uint64(f.Tables)))
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "### Cold start, %d items (load and first read; file in the page cache; median of 5 runs)\n\n", cold)
	if err := benchmarks.WriteColdStartFiles(cold, dir); err != nil {
		return err
	}
	fmt.Fprintln(w, "| Format | File | Time |")
	fmt.Fprintln(w, "|---|--:|--:|")
	for _, f := range benchmarks.ColdStarts {
		var runs []time.Duration
		var size int64
		for range 5 {
			var r [2]int64
			if err := spawn("cold:"+f, cold, dir, &r); err != nil {
				return err
			}
			runs = append(runs, time.Duration(r[0]))
			size = r[1]
		}
		slices.Sort(runs)
		fmt.Fprintf(w, "| %s | %s | %s |\n", f, mib(uint64(size)), dur(runs[len(runs)/2]))
	}
	if highCard > 0 {
		fmt.Fprintf(w, "\n### High-cardinality stress (%d items, 100%% unique Title and SKU)\n\n", highCard)
		fmt.Fprintf(w, "| Scenario | Build | Heap | Scanned heap | Full GC | Point read | Churn %d GiB: GC cycles | GC CPU |\n", benchmarks.ChurnBytes>>30)
		fmt.Fprintln(w, "|---|--:|--:|--:|--:|--:|--:|--:|")
		for _, s := range benchmarks.HighCardScenarios {
			var r benchmarks.Result
			if err := spawn("highcard:"+s, highCard, dir, &r); err != nil {
				return err
			}
			build := "-"
			if r.Build > 0 {
				build = dur(r.Build)
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %d | %s |\n",
				s, build, mib(r.Heap), mib(r.Scan), dur(r.FullGC), dur(r.Read),
				r.ChurnGCs, dur(r.ChurnGCCPU))
		}
	}
	return nil
}
