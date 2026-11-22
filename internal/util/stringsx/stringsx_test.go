package stringsx

import (
	"reflect"
	"testing"
)

func TestDedupSorted(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"trim dedupe sort", []string{" b ", "a", "b", "", "  ", "a"}, []string{"a", "b"}},
		{"already sorted", []string{"a", "b"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DedupSorted(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DedupSorted(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"order ignored", []string{"a", "b"}, []string{"b", "a"}, true},
		{"length differs", []string{"a"}, []string{"a", "b"}, false},
		{"value differs", []string{"a", "b"}, []string{"a", "c"}, false},
		{"duplicates count", []string{"a", "a", "b"}, []string{"a", "b", "b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SetEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("SetEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
