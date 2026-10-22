package utils

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"exact fit ascii", "abcdef", 6, "abcdef"},
		{"shorter than max", "abc", 10, "abc"},
		{"exact fit multibyte", "äöü", 3, "äöü"},
		{"exact fit multibyte long", "äöüäöü", 6, "äöüäöü"},
		{"long ascii", "abcdefghij", 8, "abcd ..."},
		{"max 4", "abcdefghij", 4, " ..."},
		{"max 0", "abc", 0, ""},
		{"max 1", "abc", 1, "a"},
		{"max 2", "abc", 2, "ab"},
		{"max 3", "abcdef", 3, "abc"},
		{"negative", "abc", -5, ""},
		{"empty input negative", "", -1, ""},
		{"multibyte", "äöüäöüäöü", 7, "äöü ..."},
		{"multibyte cut", "äöüäöüäöü", 6, "äö ..."},
		{"cjk", "你好世界你好世界", 6, "你好 ..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateText(tt.in, tt.max)
			assert.Equal(t, tt.want, got)
			assert.True(t, utf8.ValidString(got))
			if tt.max >= 0 {
				assert.LessOrEqual(t, utf8.RuneCountInString(got), tt.max)
			}
		})
	}
}

func TestFillWith(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		target int
		chars  string
		want   string
	}{
		{"exact fit", "abc", 3, " ", "abc"},
		{"exact fit long", "abcdef", 6, " ", "abcdef"},
		{"pad ascii", "ab", 5, ".", "ab..."},
		{"pad multibyte by runes", "é", 3, " ", "é  "},
		{"multi-rune chars cut", "a", 4, "xy", "axyx"},
		{"truncate longer", "abcdefghij", 8, " ", "abcd ..."},
		{"truncate multibyte", "äöüäöüäöü", 7, " ", "äöü ..."},
		{"target 0", "abc", 0, " ", ""},
		{"empty input", "", 3, "-", "---"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FillWith(tt.in, tt.target, tt.chars)
			assert.Equal(t, tt.want, got)
			assert.True(t, utf8.ValidString(got))
		})
	}
}

func TestFillWithEmptyChars(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- FillWith("ab", 5, "") }()
	select {
	case got := <-done:
		assert.Equal(t, "ab", got)
	case <-time.After(2 * time.Second):
		t.Fatal("FillWith with empty chars did not return")
	}
}
