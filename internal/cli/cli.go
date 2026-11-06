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
	"github.com/aloysiusvonfck/squish/internal/store"
)

const usage = `squish - shrink Docker image contents with a per-file store

Usage:
  squish store    [--store DIR] [--name NAME] [--no-elide] SOURCE...
  squish materialize [--store DIR] MANIFEST OUTDIR [--no-hardlinks] [--verify]
  squish verify   [--store DIR] MANIFEST
  squish stats    [--store DIR] MANIFEST
  squish repack   [--store DIR] MANIFEST [--max-file N] [--min-group N] [--force]

Store commands take a docker save archive (.tar or .tar.gz) or a directory.
Manifests are named by their basename unless --name is given.`

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
	fs.Parse(args)
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
	fs.Parse(args)
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
	fs.Parse(args)
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
	fs.Parse(args)
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
	fs.Parse(args)
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
