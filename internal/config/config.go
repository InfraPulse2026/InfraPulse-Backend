package config

import (
	"fmt"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Remediation              rm.Policy
	ConfigurationDriftMaxAge time.Duration
	AutomationEnabled        bool
	AutomationVault          *datasources.Vault
	CollectorConfigEnabled   bool
	AssetAutoResolution      bool
	AssetResolutionMaxAge    time.Duration
	DataSourceVault          *datasources.Vault
	DataSourceNetwork        *datasources.NetworkPolicy
	DatabaseURL              string
	HTTPAddress              string
	GRPCAddress              string
	AgentTimeout             time.Duration
	TLSCert                  string
	TLSKey                   string
	TLSClientCA              string
	MonitoringDefaults       monitoring.Policy
	TelephonyIntervalSeconds int
	Access                   access.Config
	Commands                 commands.Policy
	PKI                      pki.Config
	Health                   health.Policy
	AI                       analysisai.Config
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
	c.AssetAutoResolution, err = strconv.ParseBool(env("ASSET_AUTO_RESOLUTION_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("ASSET_AUTO_RESOLUTION_ENABLED must be true or false")
	}
	c.AssetResolutionMaxAge, err = time.ParseDuration(env("ASSET_RESOLUTION_MAX_AGE", "24h"))
	if err != nil || c.AssetResolutionMaxAge < time.Minute || c.AssetResolutionMaxAge > 30*24*time.Hour {
		return c, fmt.Errorf("ASSET_RESOLUTION_MAX_AGE must be 1m..720h")
	}
	c.DataSourceVault, err = datasources.NewVault(os.Getenv("DATA_SOURCE_ENCRYPTION_KEY"))
	if err != nil {
		return c, err
	}
	c.MonitoringDefaults = monitoring.Defaults()
	c.TelephonyIntervalSeconds = telephony.DefaultIntervalSeconds
	for _, setting := range []struct {
		key   string
		value *int
	}{
		{"DEFAULT_TELEMETRY_INTERVAL", &c.MonitoringDefaults.TelemetryIntervalSeconds},
		{"DEFAULT_INVENTORY_INTERVAL", &c.MonitoringDefaults.InventoryIntervalSeconds},
		{"DEFAULT_TOPOLOGY_INTERVAL", &c.MonitoringDefaults.TopologyIntervalSeconds},
		{"DEFAULT_TELEPHONY_INTERVAL", &c.TelephonyIntervalSeconds},
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
	c.Access.Mode = env("AUTH_MODE", "required")
	if c.Access.Mode != "required" && c.Access.Mode != "disabled" {
		return c, fmt.Errorf("AUTH_MODE must be required or disabled")
	}
	host, _, _ := net.SplitHostPort(c.HTTPAddress)
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	c.DataSourceNetwork, err = loadSourceNetwork(loopback)
	if err != nil {
		return c, err
	}
	if c.Access.Mode == "disabled" && !loopback {
		return c, fmt.Errorf("AUTH_MODE=disabled requires a numeric loopback HTTP listener")
	}
	c.Access.SessionTTL, err = time.ParseDuration(env("AUTH_SESSION_TTL", "8h"))
	if err != nil || c.Access.SessionTTL < time.Minute || c.Access.SessionTTL > 24*time.Hour {
		return c, fmt.Errorf("AUTH_SESSION_TTL must be 1m..24h")
	}
	c.Access.PublicOrigin = os.Getenv("AUTH_PUBLIC_ORIGIN")
	if c.Access.PublicOrigin != "" {
		u, e := url.Parse(c.Access.PublicOrigin)
		if e != nil || u.Host == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return c, fmt.Errorf("AUTH_PUBLIC_ORIGIN must be one exact http(s) origin without path")
		}
		c.Access.PublicOrigin = strings.ToLower(c.Access.PublicOrigin)
	}
	c.Access.CookieSecure = !loopback
	if setting := os.Getenv("AUTH_COOKIE_SECURE"); setting != "" {
		c.Access.CookieSecure, err = strconv.ParseBool(setting)
		if err != nil {
			return c, fmt.Errorf("AUTH_COOKIE_SECURE must be true or false")
		}
	}
	if !c.Access.CookieSecure && !loopback {
		u, e := url.Parse(c.Access.PublicOrigin)
		if e != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			return c, fmt.Errorf("insecure cookies on wildcard listeners require an explicit loopback public origin for local containers")
		}
	}
	c.Commands, err = LoadCommandPolicy()
	if err != nil {
		return c, err
	}
	c.PKI.Enabled, err = strconv.ParseBool(env("PKI_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("PKI_ENABLED must be true or false")
	}
	c.PKI.Address = env("PKI_ADDRESS", "127.0.0.1:9444")
	c.PKI.CAFile = os.Getenv("PKI_CA_FILE")
	c.PKI.CAKeyFile = os.Getenv("PKI_CA_KEY_FILE")
	c.PKI.CertFile = env("PKI_TLS_CERT_FILE", c.TLSCert)
	c.PKI.KeyFile = env("PKI_TLS_KEY_FILE", c.TLSKey)
	c.PKI.Lifetime, err = time.ParseDuration(env("PKI_CERT_LIFETIME", "720h"))
	if err != nil || c.PKI.Lifetime < time.Minute || c.PKI.Lifetime > 90*24*time.Hour {
		return c, fmt.Errorf("PKI_CERT_LIFETIME must be 1m..2160h")
	}
	if c.PKI.Enabled {
		_, port, e := net.SplitHostPort(c.PKI.Address)
		n, e2 := strconv.Atoi(port)
		if e != nil || e2 != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("PKI_ADDRESS must be host:port")
		}
		if c.Access.Mode != "required" || c.TLSClientCA == "" || c.PKI.CAFile == "" || c.PKI.CAKeyFile == "" || c.PKI.CertFile == "" || c.PKI.KeyFile == "" {
			return c, fmt.Errorf("PKI requires REST auth, gRPC mTLS and issuing CA/enrollment TLS credentials")
		}
	}
	c.Health = health.Defaults()
	for key, dest := range map[string]*float64{"HEALTH_CPU_WARNING": &c.Health.CPUWarning, "HEALTH_CPU_CRITICAL": &c.Health.CPUCritical, "HEALTH_CPU_RECOVERY": &c.Health.CPURecovery, "HEALTH_RAM_WARNING": &c.Health.RAMWarning, "HEALTH_RAM_CRITICAL": &c.Health.RAMCritical, "HEALTH_RAM_RECOVERY": &c.Health.RAMRecovery} {
		if value := os.Getenv(key); value != "" {
			*dest, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
		}
	}
	if value := os.Getenv("HEALTH_FORECAST_HORIZON_SECONDS"); value != "" {
		c.Health.ForecastHorizonSeconds, err = strconv.Atoi(value)
		if err != nil {
			return c, fmt.Errorf("invalid forecast horizon")
		}
	}
	if err = c.Health.Validate(); err != nil {
		return c, err
	}
	c.AI.Enabled, err = strconv.ParseBool(env("AI_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("AI_ENABLED must be true/false")
	}
	c.AI.Model = os.Getenv("AI_MODEL")
	c.AI.APIKey = os.Getenv("OPENAI_API_KEY")
	c.AI.Timeout, err = time.ParseDuration(env("AI_TIMEOUT", "45s"))
	if err != nil || c.AI.Timeout < time.Second || c.AI.Timeout > 60*time.Second {
		return c, fmt.Errorf("AI_TIMEOUT must be 1s..60s")
	}
	if c.AI.Enabled && (c.Access.Mode != "required" || c.AI.Model == "" || len(c.AI.Model) > 128 || strings.ContainsAny(c.AI.Model, "\r\n\x00") || c.AI.APIKey == "" || strings.ContainsAny(c.AI.APIKey, "\r\n\x00")) {
		return c, fmt.Errorf("AI requires REST auth, explicit AI_MODEL and OPENAI_API_KEY")
	}
	c.CollectorConfigEnabled, err = strconv.ParseBool(env("COLLECTOR_CONFIG_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("COLLECTOR_CONFIG_ENABLED must be true/false")
	}
	if c.CollectorConfigEnabled && (c.TLSCert == "" || c.TLSKey == "" || c.TLSClientCA == "" || c.Access.Mode != "required") {
		return c, fmt.Errorf("managed Collector configuration requires gRPC mTLS and REST authentication")
	}
	c.AutomationEnabled, err = strconv.ParseBool(env("AUTOMATION_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("AUTOMATION_ENABLED must be true/false")
	}
	c.AutomationVault, err = datasources.NewVault(os.Getenv("AUTOMATION_ENCRYPTION_KEY"))
	if err != nil {
		return c, fmt.Errorf("AUTOMATION_ENCRYPTION_KEY must be base64 of 32 bytes")
	}
	if c.AutomationEnabled && (c.Access.Mode != "required" || c.AutomationVault == nil) {
		return c, fmt.Errorf("automation requires REST authentication and AUTOMATION_ENCRYPTION_KEY")
	}
	c.ConfigurationDriftMaxAge, err = time.ParseDuration(env("CONFIGURATION_DRIFT_MAX_AGE", "15m"))
	if err != nil || c.ConfigurationDriftMaxAge < time.Minute || c.ConfigurationDriftMaxAge > 24*time.Hour {
		return c, fmt.Errorf("CONFIGURATION_DRIFT_MAX_AGE must be 1m..24h")
	}
	c.Remediation, err = LoadRemediationPolicy()
	if err != nil {
		return c, err
	}
	if c.Remediation.Enabled && (!c.AutomationEnabled || c.Access.Mode != "required") {
		return c, fmt.Errorf("remediation requires enabled automation and REST authentication")
	}
	return c, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
