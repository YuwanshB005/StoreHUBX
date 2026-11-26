package config

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	good := &Config{MongoURI: "mongodb://x", JWTSecret: strings.Repeat("s", 32)}
	key := strings.Repeat("k", 32)

	if err := Validate(good, key); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		cfg Config
		key string
	}{
		"empty jwt secret": {Config{MongoURI: "m"}, key},
		"short jwt secret": {Config{MongoURI: "m", JWTSecret: "short"}, key},
		"missing mongo":    {Config{JWTSecret: good.JWTSecret}, key},
		"short enc key":    {*good, "abc"},
	} {
		cfg := tc.cfg
		if err := Validate(&cfg, tc.key); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
