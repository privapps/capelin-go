package tools

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestFormatContentHashUsesCanonicalTruncatedSHA256(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "empty",
			data: nil,
			want: "sha256-128:47DEQpj8HBSa-_TImW-5JA",
		},
		{
			name: "hello",
			data: []byte("hello"),
			want: "sha256-128:LPJNul-wow4m6Dsqxbning",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatContentHash(test.data)
			if len(got) != len("sha256-128:")+base64.RawURLEncoding.EncodedLen(sha256.Size/2) {
				t.Fatalf("formatContentHash length = %d, want %d", len(got), len("sha256-128:")+22)
			}
			if !strings.HasPrefix(got, "sha256-128:") {
				t.Fatalf("formatContentHash = %q, missing canonical prefix", got)
			}
			if strings.Contains(got, "=") {
				t.Fatalf("formatContentHash = %q, must not contain padding", got)
			}
			if got != test.want {
				t.Fatalf("formatContentHash = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseContentHashAcceptsCanonicalAndLegacyFormats(t *testing.T) {
	data := []byte("hello")
	short := formatContentHash(data)
	sum := sha256.Sum256(data)
	legacy := fmt.Sprintf("sha256:%x", sum[:])

	for _, value := range []string{short, legacy} {
		t.Run(value[:strings.IndexByte(value, ':')], func(t *testing.T) {
			parsed, err := parseContentHash(value)
			if err != nil {
				t.Fatalf("parseContentHash(%q): %v", value, err)
			}
			if !contentHashMatches(parsed, data) {
				t.Fatalf("contentHashMatches(%q) = false", value)
			}
		})
	}
}

func TestParseContentHashRejectsNonCanonicalShortValues(t *testing.T) {
	valid := formatContentHash([]byte("hello"))
	for _, value := range []string{
		valid + "=",
		"sha256-128:" + strings.Repeat("+", 22),
		"sha256-128:" + strings.Repeat("A", 21),
		"md5:" + strings.Repeat("a", 32),
		"md4:" + strings.Repeat("a", 32),
	} {
		if err := validateContentHash(value); err == nil {
			t.Fatalf("validateContentHash(%q) succeeded, want error", value)
		}
	}
}
