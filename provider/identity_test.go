package provider

import (
	"reflect"
	"testing"
)

func TestUUIDFormsReturnsCanonicalAndByteSwapped(t *testing.T) {
	got := UUIDForms("421B7F1E-7B7A-3D6E-2C0E-9C4B5A6F7D8E")
	want := []string{
		"421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e",
		"1e7f1b42-7a7b-6e3d-2c0e-9c4b5a6f7d8e",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UUIDForms = %v, want %v", got, want)
	}
}

func TestUUIDFormsRejectsPlaceholders(t *testing.T) {
	for _, u := range []string{
		"",
		"00000000-0000-0000-0000-000000000000",
		"FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF",
		"03000200-0400-0500-0006-000700080009",
		"not-a-uuid",
	} {
		if got := UUIDForms(u); got != nil {
			t.Errorf("UUIDForms(%q) = %v, want nil", u, got)
		}
	}
}

// The byte-swapped form of the byte-swapped form is the original: whichever
// side reports which encoding, the two sets intersect.
func TestUUIDFormsIsAnInvolution(t *testing.T) {
	a := UUIDForms("421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e")
	b := UUIDForms(a[1])
	if b[1] != a[0] {
		t.Fatalf("swapping twice gave %q, want %q", b[1], a[0])
	}
}
