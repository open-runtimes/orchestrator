// mkimage builds an EROFS image with the in-tree writer. It is intentionally
// small so profile sweeps can run under time(1) without the artifact pipeline.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"time"

	"orchestrator/internal/erofs"
)

func main() {
	algorithm := flag.String("algorithm", "lz4hc", "lz4 or lz4hc")
	pcluster := flag.Int("pcluster", 128<<10, "maximum physical pcluster bytes")
	maxExtent := flag.Int("max-extent", 4<<20, "maximum decoded extent bytes")
	fragments := flag.Bool("fragments", true, "pack small files into the packed inode")
	packedPCluster := flag.Int("packed-pcluster", 64<<10, "packed-inode physical pcluster bytes")
	packedMaxExtent := flag.Int("packed-max-extent", 1<<20, "packed-inode maximum decoded extent bytes")
	dedupe := flag.Bool("dedupe", true, "deduplicate identical content")
	fragmentOrderFile := flag.String("fragment-order", "", "newline-separated hot paths to pack first, in access order")
	compact := flag.Bool("compact-inodes", true, "use compact inodes")
	workers := flag.Int("workers", runtime.GOMAXPROCS(0), "GOMAXPROCS")
	cpuProfile := flag.String("cpuprofile", "", "write a Go CPU profile to this file")
	memProfile := flag.String("memprofile", "", "write a Go heap profile to this file")
	traceFile := flag.String("trace", "", "write a Go execution trace to this file")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkimage [flags] SOURCE IMAGE")
		os.Exit(2)
	}

	runtime.GOMAXPROCS(*workers)
	var fragmentOrder []string
	if *fragmentOrderFile != "" {
		data, err := os.ReadFile(*fragmentOrderFile)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fragmentOrder = append(fragmentOrder, line)
			}
		}
	}
	if *cpuProfile != "" {
		profile, err := os.Create(*cpuProfile)
		if err != nil {
			panic(err)
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			panic(err)
		}
		defer func() {
			pprof.StopCPUProfile()
			if err := profile.Close(); err != nil {
				panic(err)
			}
		}()
	}
	if *memProfile != "" {
		defer func() {
			profile, err := os.Create(*memProfile)
			if err != nil {
				panic(err)
			}
			runtime.GC()
			if err := pprof.WriteHeapProfile(profile); err != nil {
				panic(err)
			}
			if err := profile.Close(); err != nil {
				panic(err)
			}
		}()
	}
	if *traceFile != "" {
		traceOut, err := os.Create(*traceFile)
		if err != nil {
			panic(err)
		}
		if err := trace.Start(traceOut); err != nil {
			panic(err)
		}
		defer func() {
			trace.Stop()
			if err := traceOut.Close(); err != nil {
				panic(err)
			}
		}()
	}
	out, err := os.Create(flag.Arg(1))
	if err != nil {
		panic(err)
	}
	opts := []erofs.CreateOpt{erofs.WithCompression(erofs.CompressionOptions{
		Algorithm:           *algorithm,
		PClusterSize:        *pcluster,
		MaxExtentSize:       *maxExtent,
		Fragments:           *fragments,
		PackedPClusterSize:  *packedPCluster,
		PackedMaxExtentSize: *packedMaxExtent,
		Dedupe:              *dedupe,
		FragmentOrder:       fragmentOrder,
	})}
	if *compact {
		opts = append(opts, erofs.WithCompactInodes())
	}

	start := time.Now()
	w := erofs.Create(out, opts...)
	if err := w.CopyFrom(os.DirFS(flag.Arg(0))); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	if err := out.Close(); err != nil {
		panic(err)
	}
	st, err := os.Stat(flag.Arg(1))
	if err != nil {
		panic(err)
	}
	fmt.Printf("bytes=%d elapsed=%s\n", st.Size(), time.Since(start))
}
