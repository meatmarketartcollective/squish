// Package cli wires the squish command line.
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aloysiusvonfck/squish/internal/ingest"
	"github.com/aloysiusvonfck/squish/internal/ops"
	"github.com/aloysiusvonfck/squish/internal/sqfs"
	"github.com/aloysiusvonfck/squish/internal/store"
)

const usage = `squish - shrink Docker image contents with a per-file store

Usage:
  squish store    [--store DIR] [--name NAME] [--no-elide] SOURCE...
  squish materialize [--store DIR] MANIFEST OUTDIR [--no-hardlinks] [--verify]
  squish verify   [--store DIR] MANIFEST
  squish stats    [--store DIR] MANIFEST
  squish repack   [--store DIR] MANIFEST [--max-file N] [--min-group N] [--force]
  squish delta    [--store DIR] [--min-file N] [--max-file N]
  squish cold     [--store DIR] [--min-size N] MANIFEST
  squish gc       [--store DIR]
  squish mount    [--store DIR] MANIFEST MNT

Store commands take a docker save archive (.tar or .tar.gz) or a directory.
Manifests are named by their basename unless --name is given.`

// flagsFirst moves flag tokens (with their values) ahead of positional
// arguments, so "repack redis --max-file 4096" parses the same as
// "repack --max-file 4096 redis".
func flagsFirst(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				flags = append(flags, a)
				continue
			}
			flags = append(flags, a)
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

var valueFlags = map[string]bool{
	"store": true, "name": true, "max-file": true, "min-group": true,
	"min-size": true, "min-file": true,
}

func Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "store":
		return cmdStore(rest)
	case "materialize":
		return cmdMaterialize(rest)
	case "verify":
		return cmdVerify(rest)
	case "stats":
		return cmdStats(rest)
	case "repack":
		return cmdRepack(rest)
	case "delta":
		return cmdDelta(rest)
	case "cold":
		return cmdCold(rest)
	case "gc":
		return cmdGC(rest)
	case "mount":
		return cmdMount(rest)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func openStore(dir string) (*store.Store, error) {
	if dir == "" {
		dir = os.Getenv("STACKFOLD_STORE")
	}
	if dir == "" {
		dir = ".squish"
	}
	return store.Open(dir)
}

func cmdStore(args []string) error {
	fs := flag.NewFlagSet("store", flag.ExitOnError)
	sdir := fs.String("store", "", "store root (default $STACKFOLD_STORE or ./.squish)")
	name := fs.String("name", "", "manifest name (default source basename)")
	noElide := fs.Bool("no-elide", false, "store regenerable files too")
	fs.Parse(flagsFirst(args))
	if fs.NArg() == 0 {
		return fmt.Errorf("store: need a source (docker save tar or directory)")
	}
	st, err := openStore(*sdir)
	if err != nil {
		return err
	}
	for _, src := range fs.Args() {
		nm := *name
		if nm == "" {
			nm = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
			nm = strings.TrimSuffix(nm, ".tar")
		}
		fmt.Printf("storing %s\n", src)
		m, err := ingest.IngestPath(st, src, nm, ingest.IngestOptions{Elide: !*noElide})
		if err != nil {
			return err
		}
		if err := st.SaveManifest(m); err != nil {
			return err
		}
		fmt.Printf("  manifest %s\n", st.ManifestPath(m.Name))
		if err := ops.Stats(st, m); err != nil {
			return err
		}
	}
	return nil
}

func loadManifestAt(sdir, ref string) (*store.Store, *store.Manifest, error) {
	st, err := openStore(sdir)
	if err != nil {
		return nil, nil, err
	}
	name := strings.TrimSuffix(filepath.Base(ref), ".json")
	m, err := st.LoadManifest(name)
	if err != nil {
		// accept a direct path too
		if b, rerr := os.ReadFile(ref); rerr == nil {
			var mm store.Manifest
			if jerr := jsonUnmarshal(b, &mm); jerr == nil {
				return st, &mm, nil
			}
		}
		return nil, nil, err
	}
	return st, m, nil
}

func cmdMaterialize(args []string) error {
	fs := flag.NewFlagSet("materialize", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	noLinks := fs.Bool("no-hardlinks", false, "copy duplicates instead of hardlinking")
	verify := fs.Bool("verify", false, "re-hash each file after writing")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 2 {
		return fmt.Errorf("materialize: need MANIFEST and OUTDIR")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	n, err := ops.Materialize(st, m, fs.Arg(1), !*noLinks, *verify)
	if err != nil {
		return err
	}
	fmt.Printf("materialized %d files into %s\n", n, fs.Arg(1))
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 1 {
		return fmt.Errorf("verify: need MANIFEST")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	checked, bad, err := ops.Verify(st, m)
	if err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d entries failed verification", bad, checked)
	}
	fmt.Printf("verified %d entries, all hashes match\n", checked)
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 1 {
		return fmt.Errorf("stats: need MANIFEST")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	return ops.Stats(st, m)
}

func cmdRepack(args []string) error {
	fs := flag.NewFlagSet("repack", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	maxFile := fs.Int("max-file", 256*1024, "only files up to this many bytes are grouped")
	minGroup := fs.Int("min-group", 8, "minimum files per group")
	force := fs.Bool("force", false, "delete blobs even when other manifests share the store")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 1 {
		return fmt.Errorf("repack: need MANIFEST")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	_, _, err = ops.Repack(st, m, *maxFile, *minGroup, *force)
	return err
}

func openStoreAt(sdir string) (*store.Store, error) {
	root := "."
	if sdir != "" {
		root = sdir
	}
	return store.Open(root)
}

func cmdDelta(args []string) error {
	fs := flag.NewFlagSet("delta", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	minFile := fs.Int("min-file", 32*1024, "only files of at least this many bytes are delta'd")
	maxFile := fs.Int("max-file", 8*1024*1024, "only files up to this many bytes are delta'd")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 0 {
		return fmt.Errorf("delta: takes no arguments")
	}
	st, err := openStoreAt(*sdir)
	if err != nil {
		return err
	}
	names, err := st.ListManifests()
	if err != nil {
		return err
	}
	if len(names) < 2 {
		return fmt.Errorf("delta: needs at least two manifests in the store (found %d)", len(names))
	}
	var ms []*store.Manifest
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			return err
		}
		ms = append(ms, m)
	}
	conv, saved, err := ops.DeltaPass(st, ms, int64(*minFile), int64(*maxFile))
	if err != nil {
		return err
	}
	removed, reclaimed, err := ops.GCAll(st)
	if err != nil {
		return err
	}
	fmt.Printf("delta: %d entries converted, ~%d bytes saved; gc removed %d objects (%d bytes)\n", conv, saved, removed, reclaimed)
	return nil
}

func cmdCold(args []string) error {
	fs := flag.NewFlagSet("cold", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	minSize := fs.Int("min-size", 1024*1024, "only files of at least this many bytes get cold frames")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 1 {
		return fmt.Errorf("cold: need MANIFEST")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	conv, saved, err := ops.ColdPass(st, m, int64(*minSize))
	if err != nil {
		return err
	}
	removed, reclaimed, err := ops.GCAll(st)
	if err != nil {
		return err
	}
	fmt.Printf("cold: %d entries converted, ~%d bytes saved; gc removed %d objects (%d bytes)\n", conv, saved, removed, reclaimed)
	return nil
}

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 0 {
		return fmt.Errorf("gc: takes no arguments")
	}
	st, err := openStoreAt(*sdir)
	if err != nil {
		return err
	}
	removed, reclaimed, err := ops.GCAll(st)
	if err != nil {
		return err
	}
	fmt.Printf("gc: removed %d objects, reclaimed %d bytes\n", removed, reclaimed)
	return nil
}

func cmdMount(args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	sdir := fs.String("store", "", "store root")
	fs.Parse(flagsFirst(args))
	if fs.NArg() != 2 {
		return fmt.Errorf("mount: need MANIFEST MNT")
	}
	st, m, err := loadManifestAt(*sdir, fs.Arg(0))
	if err != nil {
		return err
	}
	return sqfs.Mount(st, m, fs.Arg(1))
}
