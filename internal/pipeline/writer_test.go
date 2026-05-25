package pipeline

import "testing"

func TestSanitizeGroupName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Família Doe", "famlia-doe"},
		{"  Hello World  ", "hello-world"},
		{"Group #1 (cool)", "group-1-cool"},
		{"!!!", "unnamed-group"},
		{"", "unnamed-group"},
		{"Already-safe-slug", "already-safe-slug"},
		{"UPPER lower 123", "upper-lower-123"},
	}
	for _, c := range cases {
		if got := SanitizeGroupName(c.in); got != c.want {
			t.Errorf("SanitizeGroupName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeGroupName_Truncates(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := SanitizeGroupName(string(long))
	if len(got) != 64 {
		t.Fatalf("expected truncation to 64 chars, got %d", len(got))
	}
}

func TestHashMediaKey_DeterministicAndShort(t *testing.T) {
	key := []byte("test-media-key-32-bytes-padding!!")
	a := HashMediaKey(key)
	b := HashMediaKey(key)
	if a != b {
		t.Fatalf("hash should be deterministic, got %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("hash length expected 16, got %d", len(a))
	}
	other := HashMediaKey([]byte("different-key"))
	if other == a {
		t.Fatalf("different inputs collided: %q", a)
	}
}

func TestExtFromMime(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":              "jpg",
		"image/png":               "png",
		"image/webp":              "webp",
		"video/mp4":               "mp4",
		"audio/ogg; codecs=opus":  "ogg",
		"":                        "bin",
		"application/x-totally-made-up": "bin",
	}
	for mime, want := range cases {
		if got := extFromMime(mime); got != want {
			t.Errorf("extFromMime(%q) = %q, want %q", mime, got, want)
		}
	}
}
