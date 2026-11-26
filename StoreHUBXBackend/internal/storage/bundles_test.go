package storage

import "testing"

func TestKeyFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:9000/storehub/components/btn/1.0.0/index.html":     "components/btn/1.0.0/index.html",
		"https://cdn.example.com/components/btn/1.0.0/index.html?x=1":        "components/btn/1.0.0/index.html",
		"http://localhost:8080/bundles/components/btn/1.0.0/assets/a.js#top": "components/btn/1.0.0/assets/a.js",
	} {
		if got, ok := KeyFromURL(in); !ok || got != want {
			t.Errorf("KeyFromURL(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := KeyFromURL("http://example.com/other/thing"); ok {
		t.Error("expected no key for a non-bundle URL")
	}
}
