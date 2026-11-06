// Package elide knows which files are cheaper to regenerate than to store.
package elide

import "strings"

type rule struct {
	prefix string // match on "dir/" prefix
	suffix string
	any    string // substring
	reason string
}

var rules = []rule{
	{any: "__pycache__/", reason: "python bytecode cache"},
	{suffix: ".pyc", reason: "python bytecode"},
	{prefix: "var/cache/", reason: "package manager cache"},
	{prefix: "usr/share/doc/", reason: "package docs"},
	{prefix: "usr/share/locale/", reason: "translation catalogs"},
	{prefix: "usr/share/man/", reason: "man pages"},
}

// Match returns why the path is regenerable, or "" to store it.
func Match(relPath string, size int64) string {
	p := strings.TrimPrefix(relPath, "/")
	for _, r := range rules {
		if r.prefix != "" && strings.HasPrefix(p, r.prefix) {
			return r.reason
		}
		if r.suffix != "" && strings.HasSuffix(p, r.suffix) {
			return r.reason
		}
		if r.any != "" && strings.Contains(p, r.any) {
			return r.reason
		}
	}
	return ""
}
