package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestDataSourceEncryptionKeyConfig(t *testing.T) {
	t.Setenv("DATA_SOURCE_ENCRYPTION_KEY", "fixture-invalid-secret")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "fixture-invalid-secret") {
		t.Fatal("key accepted or echoed", err)
	}
	t.Setenv("DATA_SOURCE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)))
	c, err := Load()
	if err != nil || c.DataSourceVault == nil {
		t.Fatal("valid key rejected", err)
	}
	t.Setenv("DATA_SOURCE_ENCRYPTION_KEY", "")
	c, err = Load()
	if err != nil || c.DataSourceVault != nil {
		t.Fatal("optional key rejected", err)
	}
}
