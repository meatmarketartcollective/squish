// cdcbench is the E1 measurement tool: a read-only content-defined
// chunking pass that reports how many bytes whole-file deduplication
// leaves on the table.
//
// IMPORTANT: point it at UNCOMPRESSED trees (the extracted corpus that
// squish ingested), never at store objects: objects are already zstd
// compressed, so chunking them would measure nothing.
//
// Usage:
//
//	cdcbench --dir DIR [--dir DIR2 ...] [--min 2048] [--avg 8192]
//	         [--max 32768] [--skip-under N] [--json] [--workers N]
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type sliceFlag []string

func (s *sliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *sliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var dirs sliceFlag
	var min, avg, max, skipUnder, workers int
	var jsonOut bool
	flag.Var(&dirs, "dir", "directory of extracted files to chunk (repeatable)")
	flag.IntVar(&min, "min", 2048, "minimum chunk size")
	flag.IntVar(&avg, "avg", 8192, "target chunk size")
	flag.IntVar(&max, "max", 32768, "maximum chunk size")
	flag.IntVar(&skipUnder, "skip-under", 0, "skip files under this many bytes")
	flag.IntVar(&workers, "workers", runtime.NumCPU(), "worker count")
	flag.BoolVar(&jsonOut, "json", false, "emit JSON instead of text")
	flag.Parse()

	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "cdcbench: at least one --dir is required")
		os.Exit(2)
	}

	tasks := collect(dirs, int64(skipUnder))
	acc := newAccountor()
	start := time.Now()

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, t := range tasks {
		wg.Add(1)
		go func(path, group string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := benchFile(path, group, min, avg, max, acc); err != nil {
				fmt.Fprintf(os.Stderr, "cdcbench: %s: %v\n", path, err)
			}
		}(t.path, t.group)
	}
	wg.Wait()

	st := acc.Snapshot()
	st.Files = int64(len(tasks))
	st.TotalBytes = st.UniqueFileB + st.DupFileB
	st.ChunkMin, st.ChunkAvg, st.ChunkMax = min, avg, max
	st.Dirs = dirs
	elapsed := time.Since(start)

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			fmt.Fprintln(os.Stderr, "cdcbench:", err)
			os.Exit(1)
		}
		return
	}
	printText(st, elapsed)
}

func benchFile(path, group string, min, avg, max int, acc *Accountor) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	sc := NewScanner(min, avg, max)
	fileDigest := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			fileDigest.Write(buf[:n])
			sc.Feed(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	sc.Finish()

	var fsum [sha256.Size]byte
	copy(fsum[:], fileDigest.Sum(nil))
	acc.AddFile(fsum, fi.Size())
	for _, c := range sc.Chunks {
		acc.AddChunk(c, group)
	}
	return nil
}

type task struct {
	path  string
	group string
}

// group is the first path component under the walked root (the corpus
// layer directory here), the unit the group split reports on.
func collect(dirs []string, skipUnder int64) []task {
	var tasks []task
	for _, d := range dirs {
		root := d
		_ = filepath.WalkDir(d, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				fmt.Fprintf(os.Stderr, "cdcbench: %s: %v\n", p, err)
				return nil
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil && info.Size() >= skipUnder {
					rel, _ := filepath.Rel(root, p)
					group := rel
					if i := strings.IndexByte(rel, filepath.Separator); i > 0 {
						group = rel[:i]
					}
					tasks = append(tasks, task{path: p, group: group})
				}
			}
			return nil
		})
	}
	return tasks
}

func printText(s Stats, elapsed time.Duration) {
	mbs := float64(s.TotalBytes) / 1e6 / elapsed.Seconds()
	fmt.Printf("dirs                %s\n", strings.Join(s.Dirs, ", "))
	fmt.Printf("files               %d\n", s.Files)
	fmt.Printf("total bytes         %d\n", s.TotalBytes)
	fmt.Printf("whole-file dedup    unique file bytes %d (duplicate file bytes %d)\n", s.UniqueFileB, s.DupFileB)
	fmt.Printf("chunks              %d (min %d avg %d max %d)\n", s.Chunks, s.ChunkMin, s.ChunkAvg, s.ChunkMax)
	fmt.Printf("chunk dedup         unique chunk bytes %d (duplicate chunk bytes %d)\n", s.UniqueChunkB, s.DupChunkB)
	fmt.Printf("E1 headline         CDC saves %d bytes beyond whole-file dedup (%.2f%% of total)\n",
		s.UniqueFileB-s.UniqueChunkB, pct(s.UniqueFileB-s.UniqueChunkB, s.TotalBytes))
	fmt.Printf("elapsed             %s (%.0f MB/s)\n", elapsed.Round(time.Millisecond), mbs)
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}
