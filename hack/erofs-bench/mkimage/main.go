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
		for line := range strings.SplitSeq(string(data), "\n") {
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
	stats := w.CompressionStats()
	fmt.Printf("input_files=%d input_bytes=%d fragment_files=%d fragment_bytes=%d packed_logical_bytes=%d packed_physical_bytes=%d\n",
		stats.InputFiles, stats.InputBytes, stats.FragmentFiles, stats.FragmentBytes,
		stats.PackedLogicalBytes, stats.PackedPhysicalBytes)
	fmt.Printf("stored_extents=%d compressed_extents=%d raw_extents=%d encoded_bytes=%d raw_bytes=%d physical_bytes=%d padding_bytes=%d\n",
		stats.StoredExtents, stats.CompressedExtents, stats.RawExtents, stats.EncodedBytes,
		stats.RawBytes, stats.PhysicalBytes, stats.PaddingBytes)
	fmt.Printf("fragment_dedupe_files=%d fragment_dedupe_bytes=%d whole_file_dedupe_files=%d whole_file_dedupe_bytes=%d extent_dedupe_refs=%d extent_dedupe_logical_bytes=%d extent_dedupe_physical_bytes=%d\n",
		stats.FragmentDedupeFiles, stats.FragmentDedupeBytes,
		stats.WholeFileDedupeFiles, stats.WholeFileDedupeBytes,
		stats.ExtentDedupeReferences, stats.ExtentDedupeLogicalBytes,
		stats.ExtentDedupePhysicalBytes)
	fmt.Printf("segment_boundary_extents=%d segment_boundary_slack_bytes=%d compressed_index_bytes=%d metadata_bytes=%d flat_data_bytes=%d superblock_bytes=%d accounted_image_bytes=%d\n",
		stats.SegmentBoundaryExtents, stats.SegmentBoundarySlackBytes,
		stats.CompressedIndexBytes, stats.MetadataBytes, stats.FlatDataBytes,
		stats.SuperblockBytes, stats.ImageBytes)
}
