package database

import (
	"os"
	"testing"
)

func TestConfigFromEnv_Defaults(t *testing.T) {
	// Clear all relevant env vars to test defaults.
	envVars := []string{
		"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_PASSWORD",
		"POSTGRES_DB", "POSTGRES_SSLMODE", "POSTGRES_MAX_CONNS", "POSTGRES_MIN_CONNS",
	}
	for _, k := range envVars {
		t.Setenv(k, "")
	}

	cfg := ConfigFromEnv()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Host", cfg.Host, "localhost"},
		{"Port", cfg.Port, 5432},
		{"User", cfg.User, "museum"},
		{"Password", cfg.Password, "museum"},
		{"DBName", cfg.DBName, "museumdb"},
		{"SSLMode", cfg.SSLMode, "disable"},
		{"MaxConns", cfg.MaxConns, int32(20)},
		{"MinConns", cfg.MinConns, int32(2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_WithEnvVars(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "db.example.com")
	t.Setenv("POSTGRES_PORT", "5433")
	t.Setenv("POSTGRES_USER", "admin")
	t.Setenv("POSTGRES_PASSWORD", "secret")
	t.Setenv("POSTGRES_DB", "testdb")
	t.Setenv("POSTGRES_SSLMODE", "require")
	t.Setenv("POSTGRES_MAX_CONNS", "50")
	t.Setenv("POSTGRES_MIN_CONNS", "5")

	cfg := ConfigFromEnv()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Host", cfg.Host, "db.example.com"},
		{"Port", cfg.Port, 5433},
		{"User", cfg.User, "admin"},
		{"Password", cfg.Password, "secret"},
		{"DBName", cfg.DBName, "testdb"},
		{"SSLMode", cfg.SSLMode, "require"},
		{"MaxConns", cfg.MaxConns, int32(50)},
		{"MinConns", cfg.MinConns, int32(5)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		envVal     string
		setEnv     bool
		defaultVal string
		want       string
	}{
		{name: "env set", key: "TEST_GE_1", envVal: "custom", setEnv: true, defaultVal: "default", want: "custom"},
		{name: "env empty", key: "TEST_GE_2", envVal: "", setEnv: true, defaultVal: "default", want: "default"},
		{name: "env unset", key: "TEST_GE_3", envVal: "", setEnv: false, defaultVal: "fallback", want: "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(tt.key, tt.envVal)
			} else {
				os.Unsetenv(tt.key)
			}
			got := getEnvOrDefault(tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getEnvOrDefault(%q, %q) = %q, want %q", tt.key, tt.defaultVal, got, tt.want)
			}
		})
	}
}

func TestGetEnvIntOrDefault(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		envVal     string
		setEnv     bool
		defaultVal int
		want       int
	}{
		{name: "valid int", key: "TEST_GEI_1", envVal: "42", setEnv: true, defaultVal: 10, want: 42},
		{name: "zero", key: "TEST_GEI_2", envVal: "0", setEnv: true, defaultVal: 10, want: 0},
		{name: "negative", key: "TEST_GEI_3", envVal: "-5", setEnv: true, defaultVal: 10, want: -5},
		{name: "non-numeric returns default", key: "TEST_GEI_4", envVal: "abc", setEnv: true, defaultVal: 99, want: 99},
		{name: "float returns default", key: "TEST_GEI_5", envVal: "3.14", setEnv: true, defaultVal: 99, want: 99},
		{name: "empty returns default", key: "TEST_GEI_6", envVal: "", setEnv: true, defaultVal: 7, want: 7},
		{name: "unset returns default", key: "TEST_GEI_7", envVal: "", setEnv: false, defaultVal: 7, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(tt.key, tt.envVal)
			} else {
				os.Unsetenv(tt.key)
			}
			got := getEnvIntOrDefault(tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getEnvIntOrDefault(%q, %d) = %d, want %d", tt.key, tt.defaultVal, got, tt.want)
			}
		})
	}
}
