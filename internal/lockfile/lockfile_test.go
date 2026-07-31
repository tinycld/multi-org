package lockfile

import (
	"reflect"
	"testing"
)

func TestLockfile_ParseMarshalRoundTrip(t *testing.T) {
	in := OrgLockfile{"tinycld": "1.2.0", "@tinycld/mail": "0.3.1"}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: got %v, want %v", out, in)
	}
}

func TestLockfile_ParseRejectsMalformedJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"tinycld":`)); err == nil {
		t.Fatal("malformed lockfile parsed without error")
	}
	if _, err := Parse([]byte(`["tinycld"]`)); err == nil {
		t.Fatal("non-object lockfile parsed without error")
	}
}
