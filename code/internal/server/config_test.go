package server

import "testing"

// TestLoadConfigPort pins the default port and the range check on PORT.
//
// The failed-and-fixed history here is 118090: an out-of-range value that the
// flag parser rejected, but the environment path used to swallow silently,
// leaving the process on the default port while the operator connected to the
// value they configured. Both paths must fail loudly now.
func TestLoadConfigPort(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("PORT", "")
		cfg, err := LoadConfig(nil)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Port != 18090 {
			t.Fatalf("default port = %d, want 18090", cfg.Port)
		}
	})

	t.Run("env override", func(t *testing.T) {
		t.Setenv("PORT", "28080")
		cfg, err := LoadConfig(nil)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Port != 28080 {
			t.Fatalf("port = %d, want 28080", cfg.Port)
		}
	})

	for _, bad := range []string{"118090", "0", "-1", "abc"} {
		t.Run("reject "+bad, func(t *testing.T) {
			t.Setenv("PORT", bad)
			if _, err := LoadConfig(nil); err == nil {
				t.Fatalf("LoadConfig accepted PORT=%q", bad)
			}
		})
	}

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("PORT", "28080")
		cfg, err := LoadConfig([]string{"--port", "28081"})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Port != 28081 {
			t.Fatalf("port = %d, want 28081", cfg.Port)
		}
	})

	t.Run("reject flag over range", func(t *testing.T) {
		t.Setenv("PORT", "")
		if _, err := LoadConfig([]string{"--port", "118090"}); err == nil {
			t.Fatal("LoadConfig accepted --port 118090")
		}
	})
}
