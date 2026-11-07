package store

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeAllMaxRoundTrip(t *testing.T) {
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog; ", 6000))
	mx, err := EncodeAllMax(data)
	if err != nil {
		t.Fatal(err)
	}
	fa, err := EncodeAllFast(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(mx) >= len(fa) {
		t.Fatalf("max %d bytes not smaller than fast %d", len(mx), len(fa))
	}
	back, err := DecodeAll(mx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, back) {
		t.Fatal("round trip mismatch")
	}
}

func TestStoreFastSwitch(t *testing.T) {
	st, err := Open(t.TempDir() + "/s")
	if err != nil {
		t.Fatal(err)
	}
	// default: max. blob decodes fine either way.
	h, err := st.PutBlob([]byte(strings.Repeat("squish store content ", 2000)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadBlob(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2000*21 {
		t.Fatalf("round trip short: %d", len(got))
	}
	// fast store writes and reads fine too
	st2, _ := Open(t.TempDir() + "/s2")
	st2.Fast = true
	if _, err := st2.PutBlob([]byte("tiny")); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.ReadBlob(HashBytes([]byte("tiny"))); err != nil {
		t.Fatal(err)
	}
}
