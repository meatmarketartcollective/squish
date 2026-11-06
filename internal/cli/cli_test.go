package cli

import (
	"flag"
	"reflect"
	"testing"
)

func testFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("store", "", "")
	fs.String("name", "", "")
	fs.String("image", "", "")
	fs.String("out", "", "")
	fs.Int("max-file", 0, "")
	fs.Int("min-file", 0, "")
	fs.Int("min-size", 0, "")
	fs.Int("min-group", 0, "")
	fs.Int("min-size-mb", 0, "")
	fs.Bool("force", false, "")
	fs.Bool("no-elide", false, "")
	fs.Bool("keep", false, "")
	return fs
}

func TestFlagsFirst(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"redis", "--max-file", "4096"}, []string{"--max-file", "4096", "redis"}},
		{[]string{"--store", "s", "redis"}, []string{"--store", "s", "redis"}},
		{[]string{"redis", "--force"}, []string{"--force", "redis"}},
		{[]string{"a", "--store", "s", "b", "--no-elide"}, []string{"--store", "s", "--no-elide", "a", "b"}},
		{[]string{"--min-size=1000", "m"}, []string{"--min-size=1000", "m"}},
		{[]string{"a", "--image", "x", "b"}, []string{"--image", "x", "a", "b"}},
		{[]string{"a", "--out", "o", "--force"}, []string{"--out", "o", "--force", "a"}},
		{[]string{"a", "--min-size-mb", "500", "b"}, []string{"--min-size-mb", "500", "a", "b"}},
	}
	for _, c := range cases {
		if got := flagsFirst(testFlagSet(), c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("flagsFirst(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
