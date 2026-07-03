package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanConfigPath(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-config", "a.json", "-mine"}, "a.json"},
		{[]string{"-config=b.json"}, "b.json"},
		{[]string{"--config", "c.json"}, "c.json"},
		{[]string{"-mine", "-api", ":9"}, ""},
		{[]string{"-config"}, ""}, // missing value
	}
	for _, tc := range cases {
		if got := scanConfigPath(tc.args); got != tc.want {
			t.Errorf("scanConfigPath(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestLoadNodeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(`{"listen":":4000","maxpeers":3,"mine":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadNodeConfig(path)
	if c.str("listen", "x") != ":4000" {
		t.Errorf("listen = %q", c.str("listen", "x"))
	}
	if c.integer("maxpeers", 0) != 3 {
		t.Errorf("maxpeers = %d", c.integer("maxpeers", 0))
	}
	if !c.boolean("mine", false) {
		t.Error("mine should be true")
	}
	// A key absent from the file falls back to the flag default.
	if c.str("api", ":8080") != ":8080" {
		t.Error("absent key should return the default")
	}
	// No config path -> empty config -> all defaults.
	empty := loadNodeConfig("")
	if empty.str("listen", "d") != "d" || empty.integer("maxpeers", 7) != 7 {
		t.Error("empty config should return defaults")
	}
}
