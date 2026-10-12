package prometheus

import (
	"context"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"net/url"
)

func (c *client) info(ctx context.Context, t prommodel.Target, metric string) (map[string]string, error) {
	expression, err := prommodel.ScopedExpression(metric, t.Identity)
	if err != nil {
		return nil, datasources.Failure{Code: "configuration_required"}
	}
	var data queryData
	if err = c.get(ctx, "api/v1/query", url.Values{"query": {expression}, "limit": {"2"}, "timeout": {"5s"}}, &data); err != nil {
		return nil, err
	}
	if data.Type != "vector" {
		return nil, datasources.Failure{Code: "invalid_response"}
	}
	var values []series
	if json.Unmarshal(data.Result, &values) != nil {
		return nil, datasources.Failure{Code: "invalid_response"}
	}
	if len(values) == 0 || len(values) > 1 {
		return map[string]string{}, nil
	}
	if !scopedLabels(values[0].Labels, t.Identity) {
		return nil, datasources.Failure{Code: "invalid_response"}
	}
	return values[0].Labels, nil
}
func (c *client) enrich(ctx context.Context, t prommodel.Target, d *pb.Device) error {
	if c.cfg.Profile != "node_exporter" {
		return nil
	}
	uname, err := c.info(ctx, t, "node_uname_info")
	if err != nil {
		return err
	}
	dmi, err := c.info(ctx, t, "node_dmi_info")
	if err != nil {
		return err
	}
	for _, field := range []struct {
		out   *string
		value string
	}{{&d.Hostname, uname["nodename"]}, {&d.Os, uname["sysname"]}, {&d.Kernel, uname["release"]}, {&d.Architecture, uname["machine"]}, {&d.Vendor, dmi["system_vendor"]}, {&d.Model, dmi["product_name"]}, {&d.SerialNumber, dmi["product_serial"]}} {
		if *field.out != "" {
			continue
		}
		value, err := c.clean(field.value, 256)
		if err != nil {
			return err
		}
		*field.out = value
	}
	confidence := "medium"
	if c.cfg.VerifiedIdentityLabels {
		confidence = "high"
	}
	for _, fact := range []struct{ kind, value string }{{"serial", dmi["product_serial"]}, {"hardware_uuid", dmi["product_uuid"]}} {
		value, err := c.clean(fact.value, 256)
		if err != nil {
			return err
		}
		if value != "" {
			d.IdentityEvidence = append(d.IdentityEvidence, &pb.IdentityEvidence{Type: fact.kind, Value: value, Source: d.Observation.Source, Confidence: confidence})
		}
	}
	return nil
}
