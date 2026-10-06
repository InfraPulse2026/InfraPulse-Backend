package config

import (
	"fmt"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL        string
	HTTPAddress        string
	GRPCAddress        string
	AgentTimeout       time.Duration
	TLSCert            string
	TLSKey             string
	TLSClientCA        string
	MonitoringDefaults monitoring.Policy
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL: env("DATABASE_URL", "postgres://infrapulse:infrapulse_dev@localhost:5432/infrapulse?sslmode=disable"),
		HTTPAddress: env("HTTP_ADDRESS", "127.0.0.1:8080"),
		GRPCAddress: env("GRPC_ADDRESS", "127.0.0.1:9443"),
		TLSCert:     os.Getenv("TLS_CERT_FILE"), TLSKey: os.Getenv("TLS_KEY_FILE"),
		TLSClientCA: os.Getenv("TLS_CLIENT_CA_FILE"),
	}
	var err error
	c.MonitoringDefaults = monitoring.Defaults()
	for _, setting := range []struct {
		key   string
		value *int
	}{
		{"DEFAULT_TELEMETRY_INTERVAL", &c.MonitoringDefaults.TelemetryIntervalSeconds},
		{"DEFAULT_INVENTORY_INTERVAL", &c.MonitoringDefaults.InventoryIntervalSeconds},
		{"DEFAULT_TOPOLOGY_INTERVAL", &c.MonitoringDefaults.TopologyIntervalSeconds},
	} {
		if value := os.Getenv(setting.key); value != "" {
			d, parseErr := time.ParseDuration(value)
			if parseErr != nil || d < monitoring.MinIntervalSeconds*time.Second || d > monitoring.MaxIntervalSeconds*time.Second || d%time.Second != 0 {
				return c, fmt.Errorf("%s must be a whole-second duration between 3s and 24h", setting.key)
			}
			*setting.value = int(d / time.Second)
		}
	}
	c.AgentTimeout, err = time.ParseDuration(env("AGENT_TIMEOUT", "90s"))
	if err != nil || c.AgentTimeout < time.Second || c.AgentTimeout > 24*time.Hour {
		return c, fmt.Errorf("AGENT_TIMEOUT must be a duration between 1s and 24h")
	}
	for name, address := range map[string]string{"HTTP_ADDRESS": c.HTTPAddress, "GRPC_ADDRESS": c.GRPCAddress} {
		_, port, err := net.SplitHostPort(address)
		n, numberErr := strconv.Atoi(port)
		if err != nil || numberErr != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("%s must be host:port with port 1..65535", name)
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") || (c.TLSClientCA != "" && c.TLSCert == "") {
		return c, fmt.Errorf("TLS requires both certificate and key; client CA requires TLS")
	}
	return c, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
