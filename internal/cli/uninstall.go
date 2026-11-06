package cli

// uninstall is squish's safe exit. Every image the store holds is loaded
// back into docker before the store itself is removed, so removing squish
// never orphans an image whose layers were reclaimed. If anything fails,
// the store is left untouched.

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
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
		if err := os.RemoveAll(st.Root); err != nil {
			return fmt.Errorf("could not remove the empty store: %v", err)
		}
		fmt.Println("nothing stored; removed the empty store")
		return nil
	}
	// Fail before touching anything if docker is not usable: a half-way
	// uninstall is worse than none.
	if _, err := dockerImageRefs(dockerBin()); err != nil {
		return fmt.Errorf("uninstall needs a working docker to put the images back; nothing was changed: %v", err)
	}
	restored, skipped := 0, 0
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			return fmt.Errorf("uninstall stopped: manifest %s is unreadable, so its image may or may not be in docker; the store at %s is untouched: %v", n, st.Root, err)
		}
		if m.ImageConfig == "" {
			// Old store-made manifests without an image config were
			// never reclaimed: the image is still in docker, and
			// restore could not rebuild it anyway.
			skipped++
			fmt.Printf("skipping %s: stored without an image config, so its image was never reclaimed and is still in docker\n", m.Name)
			continue
		}
		fmt.Printf("restoring %s\n", m.Name)
		tags, imgPath, err := restoreManifestToDocker(st, m, "")
		if err != nil {
			msg := fmt.Sprintf("uninstall stopped: %v\nthe store at %s is untouched; images restored before this one are back in docker", err, st.Root)
			if imgPath != "" {
				msg += fmt.Sprintf("\nthe rebuilt archive for %s is at %s", m.Name, imgPath)
			}
			return fmt.Errorf("%s", msg)
		}
		fmt.Printf("  loaded back into docker as %s\n", strings.Join(tags, ", "))
		restored++
	}
	if err := os.RemoveAll(st.Root); err != nil {
		return fmt.Errorf("every image is restored, but the store could not be removed: %v", err)
	}
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf(" (%d configless manifest(s) skipped: their images were never reclaimed)", skipped)
	}
	fmt.Printf("uninstalled: %d image(s) back in docker, store at %s removed%s\n", restored, st.Root, note)
	fmt.Println("you can now delete the squish binary itself")
	return nil
}
