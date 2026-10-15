package config

import (
	"path/filepath"
	"testing"
)

// TestExampleConfigLoads keeps the shipped config.example.json honest: the
// loader rejects unknown fields, so a stray documentation key (or a typo in a
// new block) would break every fresh deployment that seeds from this file.
func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config.example.json does not load: %v", err)
	}
	if len(cfg.ServerKeys) == 0 {
		t.Fatal("example config lost its server keys")
	}
	limits, ok := cfg.Models.Limits["deepseek-v4.1-flash-free"]
	if !ok || limits.ContextWindow == nil || *limits.ContextWindow != 1000000 {
		t.Fatalf("example config lost the calibrated free-model limits: %+v", cfg.Models.Limits)
	}
	if cfg.Hardening.HoldFirstEventSeconds <= 0 || cfg.Hardening.IdleEventSeconds <= 0 {
		t.Fatalf("example config must ship armed hardening guards: %+v", cfg.Hardening)
	}
}
