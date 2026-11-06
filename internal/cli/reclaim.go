package cli

// reclaim / restore / ls: the one-command front door. `reclaim` takes
// every docker image, stores its contents, verifies the copy, and only
// then removes the original. `restore` builds a squashed docker-save
// archive back from the manifest and loads it. `ls` shows what is
// stored and what it saves.

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aloysiusvonfck/squish/internal/ingest"
	"github.com/aloysiusvonfck/squish/internal/ops"
	"github.com/aloysiusvonfck/squish/internal/store"
)

func dockerBin() string {
	if b := os.Getenv("SQUISH_DOCKER"); b != "" {
		return b
	}
	return "docker"
}

func sanitizeName(ref string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "@", "_")
	return r.Replace(ref)
}

// parseDockerSize reads docker's size column ("34.2MB", "1.2GB").
func parseDockerSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "KB", "KIB":
		return int64(n * 1024)
	case "MB", "MIB":
		return int64(n * 1024 * 1024)
	case "GB", "GIB":
		return int64(n * 1024 * 1024 * 1024)
	case "TB", "TIB":
		return int64(n * 1024 * 1024 * 1024 * 1024)
	}
	return int64(n)
}

func human(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	for _, s := range []string{"K", "M", "G", "T"} {
		v /= u
		if v < u {
			return fmt.Sprintf("%.1f%sB", v, s)
		}
	}
	return fmt.Sprintf("%.1fPB", v/u)
}

func runDocker(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

// dockerImageRefs lists tagged images newest sizes from `docker images`.
func dockerImageRefs(bin string) (map[string]int64, error) {
	out, err := runDocker(bin, "images", "--format", "{{.Repository}}:{{.Tag}}|{{.Size}}")
	if err != nil {
		return nil, err
	}
	refs := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		ref, size := parts[0], parseDockerSize(parts[1])
		if strings.Contains(ref, "<none>") {
			continue
		}
		refs[ref] = size
	}
	return refs, nil
}

// extractDockerMeta reads the image config and repo tags from a
// docker save archive so restore can rebuild a loadable image.
func extractDockerMeta(tarPath string) (config []byte, repoTags []string, err error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	// Real docker save archives name the image config <hash>.json and
	// point to it from manifest.json's Config field; older ones carry a
	// literal config.json. Read the manifest first, then the config it
	// names, falling back to config.json when there is no manifest.
	var cfg []byte
	var tags []string
	cfgName := "config.json"
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.Name), "./")
		if name != "manifest.json" || h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		var entries []struct {
			Config   string   `json:"Config"`
			RepoTags []string `json:"RepoTags"`
		}
		if json.Unmarshal(body, &entries) == nil && len(entries) > 0 {
			if entries[0].Config != "" {
				cfgName = entries[0].Config
			}
			tags = entries[0].RepoTags
		}
		break // reclaim saves one image per archive
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	tr = tar.NewReader(f)
	sawCfg := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.Name), "./")
		if name == cfgName && h.Typeflag == tar.TypeReg {
			cfg, err = io.ReadAll(tr)
			if err != nil {
				return nil, nil, err
			}
			sawCfg = true
			break
		}
	}
	if !sawCfg {
		return nil, nil, fmt.Errorf("no image config (%s) in %s", cfgName, filepath.Base(tarPath))
	}
	return cfg, tags, nil
}

func cmdReclaim(args []string) error {
	fs := flag.NewFlagSet("reclaim", flag.ExitOnError)
	sdir := fs.String("store", "", "store root (default $SQUISH_STORE or ~/.squish)")
	keep := fs.Bool("keep", false, "keep the original images after storing")
	want := fs.String("image", "", "comma-separated image refs (default: every tagged image)")
	minMB := fs.Int("min-size-mb", 0, "skip images smaller than this many MB")
	fs.Parse(flagsFirst(fs, args))

	bin := dockerBin()
	var order []string
	sizes := map[string]int64{}
	if *want != "" {
		for _, r := range strings.Split(*want, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				order = append(order, r)
			}
		}
	} else {
		var err error
		order, sizes, err = listAllImages(bin)
		if err != nil {
			return err
		}
	}
	if len(order) == 0 {
		fmt.Println("nothing to reclaim: no tagged docker images found")
		return nil
	}

	st, err := openStore(*sdir)
	if err != nil {
		return err
	}
	tmpDir := filepath.Join(st.Root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}

	var totalImage, totalStored int64
	storedCount, freedCount := 0, 0
	for _, ref := range order {
		if *minMB > 0 && parseDockerSize(fmt.Sprintf("%dMB", *minMB)) > sizes[ref] {
			fmt.Printf("skip %s: under %dMB\n", ref, *minMB)
			continue
		}
		tarPath := filepath.Join(tmpDir, sanitizeName(ref)+".tar")
		if _, err := runDocker(bin, "save", "-o", tarPath, ref); err != nil {
			fmt.Printf("skip %s: %v\n", ref, err)
			continue
		}
		name := sanitizeName(ref)
		fmt.Printf("storing %s\n", ref)
		m, err := ingest.IngestPath(st, tarPath, name, ingest.IngestOptions{Elide: true})
		if err != nil {
			os.Remove(tarPath)
			fmt.Printf("skip %s: %v\n", ref, err)
			continue
		}
		cfg, tags, metaErr := extractDockerMeta(tarPath)
		if metaErr == nil {
			var cfgCheck struct {
				Rootfs struct {
					DiffIds []string `json:"diff_ids"`
				} `json:"rootfs"`
			}
			if !json.Valid(cfg) || json.Unmarshal(cfg, &cfgCheck) != nil || cfgCheck.Rootfs.DiffIds == nil {
				metaErr = fmt.Errorf("the image config is not a valid docker image config")
			} else if hash, err := st.PutBlob(cfg); err == nil {
				m.ImageConfig = hash
			} else {
				metaErr = err
			}
			if len(tags) > 0 {
				m.RepoTags = tags
			}
		}
		m.Source = ref
		if err := st.SaveManifest(m); err != nil {
			return err
		}
		if metaErr != nil {
			fmt.Printf("HELD %s: the image config could not be read (%v); the image is kept, nothing was removed\n", ref, metaErr)
			os.Remove(tarPath)
			continue
		}
		_, bad, verr := ops.Verify(st, m)
		if verr != nil || bad > 0 {
			fmt.Printf("HELD %s: the stored copy did not verify, image kept\n", ref)
			continue
		}
		stored := m.StoredBytes
		img := sizes[ref]
		if img == 0 {
			if fi, err := os.Stat(tarPath); err == nil {
				img = fi.Size()
			}
		}
		pct := 0
		if img > 0 {
			pct = int(100 - 100*stored/img)
		}
		fmt.Printf("  stored %s: %s image content -> %s in the store (%d%% off)\n", name, human(img), human(stored), pct)
		totalImage += img
		totalStored += stored
		storedCount++
		os.Remove(tarPath)
		if !*keep {
			if _, err := runDocker(bin, "rmi", ref); err != nil {
				fmt.Printf("  could not remove the original: %v\n", err)
			} else {
				freedCount++
				fmt.Printf("  removed %s, %s back\n", ref, human(img))
			}
		}
	}
	fmt.Printf("----\n%d image(s) stored, %s of image content now holds %s in the store\n", storedCount, human(totalImage), human(totalStored))
	if freedCount > 0 {
		fmt.Printf("%d original(s) removed after verification\n", freedCount)
	}
	fmt.Println("bring one back with: squish restore <name>   (squish ls lists names)")
	return nil
}

func listAllImages(bin string) ([]string, map[string]int64, error) {
	refs, err := dockerImageRefs(bin)
	if err != nil {
		return nil, nil, fmt.Errorf("docker not usable: %v", err)
	}
	order := make([]string, 0, len(refs))
	for r := range refs {
		order = append(order, r)
	}
	return order, refs, nil
}

// readEntryContent returns an entry's bytes, reusing the store readers.
func readEntryContent(st *store.Store, e store.FileEntry) ([]byte, error) {
	switch e.Kind {
	case store.KindFile:
		return st.ReadBlob(e.Hash)
	case store.KindFrame:
		return st.ReadFrameFile(e.Frame, e.Offset, e.Clen)
	case store.KindDelta:
		return st.ReadDelta(e.Hash, e.Base)
	case store.KindCold:
		return st.ReadColdEntry(&e)
	}
	return nil, nil // dir, symlink, hardlink, elided, special: no content
}

// writeSquashLayer writes the flattened manifest as one uncompressed
// layer tar, returning its sha256 digest (docker's diff_id format).
func writeSquashLayer(st *store.Store, m *store.Manifest, path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tw := tar.NewWriter(f)

	written := map[string]bool{}
	var links []store.FileEntry
	writeEntry := func(e store.FileEntry) error {
		hdr := &tar.Header{Name: e.Path, Mode: int64(e.Mode), ModTime: time.Unix(0, 0)}
		switch e.Kind {
		case store.KindDir:
			hdr.Typeflag = tar.TypeDir
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case store.KindSymlink:
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.Link
			hdr.Mode = 0o777
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case store.KindFile, store.KindFrame, store.KindDelta, store.KindCold:
			data, err := readEntryContent(st, e)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(data))
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		case store.KindLink:
			links = append(links, e) // after their targets
		}
		written[e.Path] = true
		return nil
	}
	for _, e := range m.Files {
		if e.Kind == store.KindLink {
			continue
		}
		if err := writeEntry(e); err != nil {
			return "", err
		}
	}
	for _, le := range links {
		if !written[le.Link] {
			return "", fmt.Errorf("%s: hardlink target %q is missing from this image", le.Path, le.Link)
		}
		hdr := &tar.Header{Name: le.Path, Typeflag: tar.TypeLink, Linkname: le.Link, Mode: int64(le.Mode), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// hash the finished archive: its padding and end marker are part
	// of what docker will hash into the diff_id
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	sdir := fs.String("store", "", "store root (default $SQUISH_STORE or ~/.squish)")
	out := fs.String("out", "", "leave the rebuilt image archive here on failure (default inside the store)")
	fs.Parse(flagsFirst(fs, args))
	if fs.NArg() == 0 {
		return fmt.Errorf("restore: name the image to bring back (squish ls lists them)")
	}

	st, err := openStore(*sdir)
	if err != nil {
		return err
	}
	bin := dockerBin()
	tmpDir := filepath.Join(st.Root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	for _, name := range fs.Args() {
		m, err := st.LoadManifest(name)
		if err != nil {
			if m2, err2 := st.LoadManifest(sanitizeName(name)); err2 == nil {
				m = m2
			} else {
				return fmt.Errorf("restore %s: not in the store (squish ls lists names)", name)
			}
		}
		if m.ImageConfig == "" {
			return fmt.Errorf("restore %s: this manifest was stored by an older squish without image config; re-reclaim it", m.Name)
		}
		fmt.Printf("restoring %s\n", m.Name)
		layerPath := filepath.Join(tmpDir, m.Name+"-layer.tar")
		diffID, err := writeSquashLayer(st, m, layerPath)
		if err != nil {
			return err
		}
		cfgRaw, err := st.ReadBlob(m.ImageConfig)
		if err != nil {
			return err
		}
		var cfg map[string]any
		if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
			return fmt.Errorf("restore %s: stored image config is not valid JSON: %v", m.Name, err)
		}
		cfg["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{diffID}}
		cfg["history"] = []map[string]string{{
			"created":    time.Now().UTC().Format(time.RFC3339),
			"created_by": "squish restore (squashed)",
		}}
		cfgOut, _ := json.Marshal(cfg)
		tags := m.RepoTags
		if len(tags) == 0 {
			tags = []string{strings.ReplaceAll(m.Name, "_", ":")}
		}
		imgPath := *out
		if imgPath == "" {
			imgPath = filepath.Join(tmpDir, m.Name+"-image.tar")
		}
		if err := writeDockerSaveArchive(imgPath, cfgOut, layerPath, tags); err != nil {
			return err
		}
		if _, err := runDocker(bin, "load", "-i", imgPath); err != nil {
			fmt.Printf("docker load failed: %v\nthe rebuilt archive is at %s (load it by hand or retry)\n", err, imgPath)
			continue
		}
		os.Remove(imgPath)
		os.Remove(layerPath)
		fmt.Printf("  loaded back into docker as %s\n", strings.Join(tags, ", "))
	}
	return nil
}

// writeDockerSaveArchive assembles config.json, the squashed layer and
// manifest.json into a docker-loadable archive.
func writeDockerSaveArchive(path string, config []byte, layerPath string, repoTags []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()
	type entry struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	meta, _ := json.Marshal([]entry{{Config: "config.json", RepoTags: repoTags, Layers: []string{"squish/layer.tar"}}})

	hdr := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Size: size, Mode: 0o644, ModTime: time.Unix(0, 0)}
	}
	if err := tw.WriteHeader(hdr("config.json", int64(len(config)))); err != nil {
		return err
	}
	if _, err := tw.Write(config); err != nil {
		return err
	}
	lf, err := os.Open(layerPath)
	if err != nil {
		return err
	}
	lsz, _ := lf.Seek(0, io.SeekEnd)
	if _, err := lf.Seek(0, io.SeekStart); err != nil {
		lf.Close()
		return err
	}
	if err := tw.WriteHeader(hdr("squish/layer.tar", lsz)); err != nil {
		lf.Close()
		return err
	}
	if _, err := io.Copy(tw, lf); err != nil {
		lf.Close()
		return err
	}
	lf.Close()
	if err := tw.WriteHeader(hdr("manifest.json", int64(len(meta)))); err != nil {
		return err
	}
	_, err = tw.Write(meta)
	return err
}

func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	sdir := fs.String("store", "", "store root (default $SQUISH_STORE or ~/.squish)")
	fs.Parse(flagsFirst(fs, args))
	st, err := openStore(*sdir)
	if err != nil {
		return err
	}
	names, err := st.ListManifests()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("nothing stored yet (squish reclaim does it in one command)")
		return nil
	}
	fmt.Printf("%-32s %-10s %-10s %s\n", "NAME", "RAW", "STORED", "SAVED")
	var tRaw, tStored int64
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			fmt.Printf("%-32s unreadable: %v\n", n, err)
			continue
		}
		pct := 0
		if m.RawBytes > 0 {
			pct = int(100 - 100*m.StoredBytes/m.RawBytes)
		}
		fmt.Printf("%-32s %-10s %-10s %d%%\n", m.Name, human(m.RawBytes), human(m.StoredBytes), pct)
		tRaw += m.RawBytes
		tStored += m.StoredBytes
	}
	tp := 0
	if tRaw > 0 {
		tp = int(100 - 100*tStored/tRaw)
	}
	fmt.Printf("%-32s %-10s %-10s %d%%\n", "TOTAL", human(tRaw), human(tStored), tp)
	return nil
}
