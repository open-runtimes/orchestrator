// readtree measures real kernel-mounted tree reads. Build it for Linux and
// run it with enough privilege to drop the guest page cache between samples.
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	mode := flag.String("mode", "random", "random or sequential")
	root := flag.String("root", "/mnt/img", "mounted filesystem root")
	source := flag.String("source", "", "parallel source tree used to collect paths before dropping caches")
	limit := flag.Int("limit", 50000, "maximum files to read (0 means all)")
	readSize := flag.Int64("read-size", 4096, "bytes per random file (0 means whole file)")
	seed := flag.Int64("seed", 1, "random shuffle seed")
	dropCaches := flag.Bool("drop-caches", false, "drop Linux page caches after collecting paths")
	flag.Parse()
	if *mode != "random" && *mode != "sequential" {
		fmt.Fprintln(os.Stderr, "mode must be random or sequential")
		os.Exit(2)
	}

	listRoot := *root
	if *source != "" {
		listRoot = *source
	}
	var paths []string
	err := filepath.WalkDir(listRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(listRoot, path)
			if err != nil {
				return err
			}
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	if *mode == "random" {
		rng := rand.New(rand.NewSource(*seed))
		rng.Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })
	} else {
		slices.Sort(paths)
	}
	if *limit > 0 && len(paths) > *limit {
		paths = paths[:*limit]
	}
	if *dropCaches {
		unix.Sync()
		if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0); err != nil {
			panic(err)
		}
	}

	buf := make([]byte, 128<<10)
	var total int64
	start := time.Now()
	for _, rel := range paths {
		f, err := os.Open(filepath.Join(*root, rel))
		if err != nil {
			panic(err)
		}
		var n int64
		if *mode == "random" && *readSize > 0 {
			n, err = io.CopyBuffer(io.Discard, io.LimitReader(f, *readSize), buf)
		} else {
			n, err = io.CopyBuffer(io.Discard, f, buf)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			panic(err)
		}
		total += n
	}
	elapsed := time.Since(start)
	fmt.Printf("mode=%s files=%d bytes=%d elapsed_ms=%.3f files_s=%.1f mib_s=%.1f\n",
		*mode, len(paths), total, float64(elapsed.Microseconds())/1000,
		float64(len(paths))/elapsed.Seconds(), float64(total)/(1<<20)/elapsed.Seconds())
}
