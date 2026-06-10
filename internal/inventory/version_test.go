package inventory

import (
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Version
		wantErr bool
	}{
		{name: "minor only", in: "1.34", want: Version{Major: 1, Minor: 34}},
		{name: "v prefix", in: "v1.34", want: Version{Major: 1, Minor: 34}},
		{name: "v prefix with patch", in: "v1.34.2", want: Version{Major: 1, Minor: 34}},
		{name: "patch no prefix", in: "1.34.2", want: Version{Major: 1, Minor: 34}},
		{name: "zero minor", in: "1.0", want: Version{Major: 1, Minor: 0}},
		{name: "double digit everywhere", in: "v10.27.11", want: Version{Major: 10, Minor: 27}},
		{name: "empty", in: "", wantErr: true},
		{name: "just v", in: "v", wantErr: true},
		{name: "major only", in: "1", wantErr: true},
		{name: "too many parts", in: "1.34.2.7", wantErr: true},
		{name: "trailing dot", in: "1.34.", wantErr: true},
		{name: "non-numeric minor", in: "1.x", wantErr: true},
		{name: "negative minor", in: "1.-34", wantErr: true},
		{name: "plus-signed minor", in: "1.+34", wantErr: true},
		{name: "word garbage", in: "latest", wantErr: true},
		{name: "vendor suffix rejected", in: "v1.34.2-gke.100", wantErr: true},
		{name: "leading space", in: " 1.34", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseVersion(%q) = %v, want error", tt.in, got)
				}
				if !strings.Contains(err.Error(), "invalid kubernetes version") {
					t.Errorf("error %q not descriptive", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		v    Version
		want string
	}{
		{Version{Major: 1, Minor: 34}, "1.34"},
		{Version{Major: 1, Minor: 0}, "1.0"},
		{Version{Major: 2, Minor: 5}, "2.5"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%#v.String() = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		name string
		v, o Version
		want int
	}{
		{"equal", Version{1, 34}, Version{1, 34}, 0},
		{"minor less", Version{1, 33}, Version{1, 34}, -1},
		{"minor greater", Version{1, 35}, Version{1, 34}, 1},
		{"major wins over minor", Version{2, 0}, Version{1, 99}, 1},
		{"major less", Version{1, 99}, Version{2, 0}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Compare(tt.o); got != tt.want {
				t.Errorf("%v.Compare(%v) = %d, want %d", tt.v, tt.o, got, tt.want)
			}
		})
	}
}

func TestVersionNext(t *testing.T) {
	tests := []struct {
		v, want Version
	}{
		{Version{1, 34}, Version{1, 35}},
		{Version{1, 0}, Version{1, 1}},
	}
	for _, tt := range tests {
		if got := tt.v.Next(); got != tt.want {
			t.Errorf("%v.Next() = %v, want %v", tt.v, got, tt.want)
		}
	}
}
