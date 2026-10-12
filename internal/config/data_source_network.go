package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

func networkList(key string) ([]string, error) {
	raw := os.Getenv(key)
	items := []string{}
	if len(raw) > 32768 {
		return nil, errors.New("data source network allowlist is too large")
	}
	if raw == "" {
		return items, nil
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, errors.New("data source network allowlist contains an empty entry")
		}
		items = append(items, item)
	}
	return items, nil
}

func loadSourceNetwork(loopbackListener bool) (*datasources.NetworkPolicy, error) {
	c := datasources.NetworkConfig{CABundles: map[string][]byte{}}
	var err error
	c.AllowedOrigins, err = networkList("DATA_SOURCE_ALLOWED_ORIGINS")
	if err != nil {
		return nil, err
	}
	c.AllowedCIDRs, err = networkList("DATA_SOURCE_ALLOWED_CIDRS")
	if err != nil {
		return nil, err
	}
	if raw := os.Getenv("DATA_SOURCE_ALLOW_LOOPBACK"); raw != "" {
		c.AllowLoopback, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, errors.New("DATA_SOURCE_ALLOW_LOOPBACK must be true or false")
		}
	}
	if c.AllowLoopback && !loopbackListener {
		return nil, errors.New("DATA_SOURCE_ALLOW_LOOPBACK requires a numeric loopback HTTP listener")
	}
	if raw := os.Getenv("DATA_SOURCE_CA_FILES"); raw != "" {
		var files map[string]string
		if len(raw) > 16384 || json.Unmarshal([]byte(raw), &files) != nil || files == nil || len(files) > 32 {
			return nil, errors.New("DATA_SOURCE_CA_FILES must be a JSON object of CA references to deployment file paths")
		}
		for ref, path := range files {
			if path == "" {
				return nil, errors.New("data source CA file path is empty")
			}
			file, e := os.Open(path)
			if e != nil {
				return nil, errors.New("cannot open configured data source CA file")
			}
			bundle, e := io.ReadAll(io.LimitReader(file, (1<<20)+1))
			_ = file.Close()
			if e != nil || len(bundle) > 1<<20 {
				return nil, errors.New("cannot read data source CA file or file exceeds 1 MiB")
			}
			c.CABundles[ref] = bundle
		}
	}
	return datasources.NewNetworkPolicy(c)
}
