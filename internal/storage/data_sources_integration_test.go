package storage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

func TestDataSourcesIsolationEncryptionRevisionAndRevocation(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	vault, err := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureDataSources(ctx, vault); err != nil {
		t.Fatal(err)
	}
	secret := "private-fixture-token"
	in := datasources.Input{Type: "ZABBIX", Name: "PROD", Endpoint: "https://monitor.example", Enabled: true, SyncIntervalSeconds: 3, Credential: &secret}
	prod, err := s.SaveDataSource(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	in.Name = "TEST"
	test, err := s.SaveDataSource(ctx, "", in)
	if err != nil || prod.ID == test.ID {
		t.Fatal("instances collided", err)
	}
	if !prod.HasCredential || prod.ConnectionStatus != "adapter_unavailable" || len(prod.Capabilities) != 0 {
		t.Fatal("fake adapter success", prod)
	}
	public, err := json.Marshal(prod)
	if err != nil || bytes.Contains(public, []byte(secret)) {
		t.Fatal("credential leaked through public model", err)
	}
	var cipher []byte
	if err = s.pool.QueryRow(ctx, `SELECT credential_ciphertext FROM data_sources WHERE id=$1`, prod.ID).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte(secret)) {
		t.Fatal("stored plaintext")
	}
	if _, err = vault.Open(test.ID, cipher); err == nil {
		t.Fatal("credential crossed source boundary")
	}
	in.Revision = prod.Revision
	in.Credential = nil
	in.Name = "PROD-renamed"
	// B2.1/B2.2 stored URLs without the explicit default port. Canonicalization
	// during B2.3 must retain credentials when the endpoint is still identical.
	if _, err = s.pool.Exec(ctx, `UPDATE data_sources SET endpoint='https://monitor.example' WHERE id=$1`, prod.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.SaveDataSource(ctx, prod.ID, in)
	if err != nil || updated.Revision != 2 || !updated.HasCredential {
		t.Fatal("update lost credential", err)
	}
	if _, err = s.SaveDataSource(ctx, prod.ID, in); !errors.Is(err, datasources.ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	in.Revision = updated.Revision
	in.Endpoint = "https://different.example"
	if _, err = s.SaveDataSource(ctx, prod.ID, in); err != datasources.ErrInvalid {
		t.Fatal("retained secret moved to another endpoint", err)
	}
	empty := ""
	in.Credential = &empty
	updated, err = s.SaveDataSource(ctx, prod.ID, in)
	if err != nil || updated.HasCredential {
		t.Fatal("explicit secret removal failed", err)
	}
	var audits int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM data_source_audit WHERE source_id=$1`, prod.ID).Scan(&audits); err != nil || audits != 3 {
		t.Fatal("failed updates audited as success", audits, err)
	}
	if err = s.ConfigureDataSources(ctx, nil); err == nil {
		t.Fatal("missing key accepted with TEST credential")
	}
	wrong, _ := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)))
	if err = s.ConfigureDataSources(ctx, wrong); err == nil {
		t.Fatal("wrong startup key accepted")
	}
	if err = s.ConfigureDataSources(ctx, vault); err != nil {
		t.Fatal(err)
	}
	if err = s.AccessLogout(ctx, admin, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	in.Revision = 0
	if _, err = s.SaveDataSource(ctx, "", in); err != access.ErrForbidden {
		t.Fatal("revoked admin mutated source", err)
	}
}

func TestDataSourceCollectorStatusAndMigration(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	in := datasources.Input{Type: "COLLECTOR", Name: "Collector", ConnectionRef: "agent-1", Enabled: true, SyncIntervalSeconds: 3}
	v, err := s.SaveDataSource(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveDataSource(ctx, "", in); err != datasources.ErrConflict {
		t.Fatal("duplicate stream instance", err)
	}
	if err = s.Ingest(ctx, registration("agent-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.ConnectionStatus != "connected" {
		t.Fatal("stream not visible", v, err)
	}
	list, err := s.DataSources(ctx, 50, 0)
	if err != nil || len(list) != 1 || list[0].ConnectionStatus != "connected" {
		t.Fatal("list/status disagree", list, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET last_seen=now()-interval '10 minutes' WHERE agent_id='agent-1'`); err != nil {
		t.Fatal(err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.ConnectionStatus != "disconnected" {
		t.Fatal("expired heartbeat reported connected", v, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("idempotent migration", err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.ConnectionStatus != "disconnected" {
		t.Fatal("restart retained false connection", v, err)
	}
	items, err := s.DataSources(ctx, 50, 0)
	if err != nil || len(items) != 1 {
		t.Fatal("migration lost source", err)
	}
	in.Revision = v.Revision
	in.Enabled = false
	v, err = s.SaveDataSource(ctx, v.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.ConnectionStatus != "disabled" {
		t.Fatal(v, err)
	}
}
