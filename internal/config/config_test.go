package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoad(t *testing.T) {
	// 1. Test Defaults
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("failed to load default config: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected default listen addr :8080, got %s", cfg.ListenAddr)
	}
	if cfg.Workers != 4 {
		t.Errorf("expected default workers 4, got %d", cfg.Workers)
	}

	// 2. Test File Override
	tmpDir, _ := os.MkdirTemp("", "config-test")
	defer os.RemoveAll(tmpDir)
	configPath := filepath.Join(tmpDir, "config.yaml")

	yamlData := `
listen_addr: ":9090"
workers: 8
`
	os.WriteFile(configPath, []byte(yamlData), 0644)

	cfg, err = Load(configPath)
	if err != nil {
		t.Fatalf("failed to load config from file: %v", err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("expected file override listen addr :9090, got %s", cfg.ListenAddr)
	}
	if cfg.Workers != 8 {
		t.Errorf("expected file override workers 8, got %d", cfg.Workers)
	}

	// 3. Test Env Override
	t.Setenv("LOADSTAR_LISTEN_ADDR", ":9999")

	cfg, err = Load(configPath)
	if err != nil {
		t.Fatalf("failed to load config with env override: %v", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("expected env override listen addr :9999, got %s", cfg.ListenAddr)
	}
	// Workers should still be 8 from the file
	if cfg.Workers != 8 {
		t.Errorf("expected file value for workers to be preserved, got %d", cfg.Workers)
	}
}

func TestConfig_Validate(t *testing.T) {
	ok := &Config{Workers: 4, QueueDepth: 10, TimeoutS: 60}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	for name, c := range map[string]*Config{
		"zero workers":     {Workers: 0, QueueDepth: 10, TimeoutS: 60},
		"negative workers": {Workers: -1, QueueDepth: 10, TimeoutS: 60},
		"negative queue":   {Workers: 1, QueueDepth: -1, TimeoutS: 60},
		"zero timeout":     {Workers: 1, QueueDepth: 10, TimeoutS: 0},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestConfig_NegativeEnvRejectedByLoad(t *testing.T) {
	t.Setenv("LOADSTAR_QUEUE_DEPTH", "-1")
	if _, err := Load(""); err == nil {
		t.Error("expected Load to reject negative queue_depth")
	}
}

func TestConfig_NumericEnvOverride(t *testing.T) {
	t.Setenv("LOADSTAR_WORKERS", "6")
	t.Setenv("LOADSTAR_TIMEOUT_S", "45")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Workers != 6 || cfg.TimeoutS != 45 {
		t.Errorf("env override not applied: workers=%d timeout=%d", cfg.Workers, cfg.TimeoutS)
	}
}

func TestConfig_AllowInsecureExactMatch(t *testing.T) {
	t.Setenv("LOADSTAR_ALLOW_INSECURE", "TRUE") // only lowercase "true" should enable
	cfg, _ := Load("")
	if cfg.AllowInsecure {
		t.Error(`"TRUE" should not enable insecure mode (only exact "true")`)
	}
	t.Setenv("LOADSTAR_ALLOW_INSECURE", "true")
	cfg, _ = Load("")
	if !cfg.AllowInsecure {
		t.Error(`"true" should enable insecure mode`)
	}
}
