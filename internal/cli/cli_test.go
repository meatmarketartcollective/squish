package cli

import (
	"reflect"
	"testing"
)

func TestFlagsFirst(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"redis", "--max-file", "4096"}, []string{"--max-file", "4096", "redis"}},
		{[]string{"--store", "s", "redis"}, []string{"--store", "s", "redis"}},
		{[]string{"redis", "--force"}, []string{"--force", "redis"}},
		{[]string{"a", "--store", "s", "b", "--no-elide"}, []string{"--store", "s", "--no-elide", "a", "b"}},
		{[]string{"--min-size=1000", "m"}, []string{"--min-size=1000", "m"}},
	}
	for _, c := range cases {
		if got := flagsFirst(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("flagsFirst(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
