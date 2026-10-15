package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/config"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prometheus"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/safelog"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/zabbix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	log := slog.New(safelog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("backend stopped", "phase", safelog.Phase(err), "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return safelog.At("configuration", err)
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	store, err := storage.Open(startup, cfg.DatabaseURL, cfg.AgentTimeout, cfg.MonitoringDefaults, cfg.TelephonyIntervalSeconds)
	if err != nil {
		return safelog.At("database_connect", err)
	}
	defer store.Close()
	if err = store.Migrate(startup); err != nil {
		return safelog.At("database_migrate", err)
	}
	store.ConfigureCommands(cfg.Commands)
	store.ConfigureAssetResolution(cfg.AssetAutoResolution, cfg.AssetResolutionMaxAge)
	if err = store.ConfigureDataSources(startup, cfg.DataSourceVault); err != nil {
		return err
	}
	sourceRegistry, err := store.CollectorOperations().With(zabbix.New(cfg.DataSourceNetwork).Operations()...)
	if err != nil {
		return err
	}
	store.ConfigureDataSourceOperations(sourceRegistry)
	prometheusConnector := prometheus.New(cfg.DataSourceNetwork)
	sourceRegistry, err = sourceRegistry.With(prometheusConnector.Operations()...)
	if err != nil {
		return err
	}
	store.ConfigureDataSourceOperations(sourceRegistry)
	store.ConfigurePrometheus(prometheusConnector)
	if err = store.ConfigureHealth(cfg.Health); err != nil {
		return err
	}
	if err = store.RecoverCommands(startup); err != nil {
		return safelog.At("command_recovery", err)
	}
	if err = store.RecoverJobs(startup); err != nil {
		return safelog.At("analysis_recovery", err)
	}
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(4 << 20), grpc.MaxSendMsgSize(4 << 20)}
	if cfg.TLSCert != "" {
		tlsConfig, err := serverTLS(cfg)
		if err != nil {
			return err
		}
		options = append(options, grpc.Creds(credentials.NewTLS(tlsConfig)))
	} else {
		log.Warn("development gRPC without TLS; use local published ports only")
	}
	g := grpc.NewServer(options...)
	var pkiService *pki.Service
	var enrollmentTLS *tls.Config
	if cfg.PKI.Enabled {
		caPEM, e := os.ReadFile(cfg.PKI.CAFile)
		if e != nil {
			return fmt.Errorf("read issuing CA: %w", e)
		}
		keyPEM, e := os.ReadFile(cfg.PKI.CAKeyFile)
		if e != nil {
			return fmt.Errorf("read issuing key: %w", e)
		}
		issuer, e := pki.NewIssuer(caPEM, keyPEM, cfg.PKI.Lifetime)
		if e != nil {
			return e
		}
		clientCA, e := os.ReadFile(cfg.TLSClientCA)
		if e != nil {
			return e
		}
		if e = pki.CheckTrust(caPEM, clientCA); e != nil {
			return e
		}
		pair, e := tls.LoadX509KeyPair(cfg.PKI.CertFile, cfg.PKI.KeyFile)
		if e != nil {
			return e
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		enrollmentTLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven}
		pkiService = &pki.Service{Repository: store, Issuer: issuer}
	}
	store.ConfigureCollectorConfig(cfg.CollectorConfigEnabled)
	if err = store.ConfigureAutomation(startup, cfg.AutomationEnabled, cfg.AutomationVault); err != nil {
		return err
	}
	if err = store.ConfigureConfigurationDrift(cfg.ConfigurationDriftMaxAge); err != nil {
		return err
	}
	controllerOptions := []controller.Option{controller.WithManagedConfiguration(cfg.CollectorConfigEnabled)}
	if pkiService != nil {
		controllerOptions = append(controllerOptions, controller.WithAgentAuthorizer(pkiService.AuthorizePeer))
	} else if cfg.TLSClientCA != "" {
		controllerOptions = append(controllerOptions, controller.WithAgentAuthorizer(pki.AuthorizePeer))
	}
	control := controller.New(store, log, controllerOptions...)
	if pkiService != nil {
		pkiService.OnRevoke = control.DisconnectAgent
	}
	pb.RegisterAgentServiceServer(g, control)
	workerCtx, workerStop := context.WithCancel(ctx)
	defer workerStop()
	configDone := make(chan struct{})
	go func() { defer close(configDone); collectorconfig.Run(workerCtx, store, control, log) }()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); commands.Run(workerCtx, store, control, log) }()
	collectorDone := make(chan struct{})
	topologyDone := make(chan struct{})
	go func() { defer close(topologyDone); topology.RunExpiry(workerCtx, store, log) }()
	go func() { defer close(collectorDone); health.RunCollectors(workerCtx, store, log) }()
	correlationDone := make(chan struct{})
	go func() { defer close(correlationDone); correlation.Run(workerCtx, store, log) }()
	reportDone := make(chan struct{})
	go func() { defer close(reportDone); reports.RunScheduled(workerCtx, store, log) }()
	aiDone := make(chan struct{})
	sourceDone := make(chan struct{})
	go func() { defer close(sourceDone); datasources.Run(workerCtx, store, sourceRegistry, log) }()
	go func() {
		defer close(aiDone)
		analysisai.Run(workerCtx, store, analysisai.NewResponsesProvider(cfg.AI), cfg.AI, log)
	}()
	grpcListener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		return err
	}
	defer grpcListener.Close()
	httpListener, err := net.Listen("tcp", cfg.HTTPAddress)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	apiOptions := []api.Option{api.WithAccess(access.New(store, cfg.Access)), api.WithCommands(store), api.WithIncidents(store), api.WithCorrelation(store), api.WithReports(store), api.WithAI(store, cfg.AI), api.WithDataSources(store), api.WithAssets(store), api.WithCollectorConfig(store), api.WithAutomation(store), api.WithConfigurationDrift(store)}
	if pkiService != nil {
		apiOptions = append(apiOptions, api.WithPKI(pkiService))
	}
	if cfg.Access.Mode == "disabled" {
		apiOptions = append(apiOptions, api.WithDevelopmentAuthDisabled())
		log.Warn("HTTP authentication explicitly disabled on loopback")
	}
	h := &http.Server{Handler: api.New(store, log, apiOptions...), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	results := make(chan error, 3)
	var enrollmentServer *http.Server
	if pkiService != nil {
		listener, e := net.Listen("tcp", cfg.PKI.Address)
		if e != nil {
			return e
		}
		defer listener.Close()
		enrollmentServer = &http.Server{Handler: pkiService.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() { results <- enrollmentServer.Serve(tls.NewListener(listener, enrollmentTLS)) }()
		log.Info("PKI enrollment listener ready", "https", cfg.PKI.Address)
	}
	go func() { results <- g.Serve(grpcListener) }()
	go func() { results <- h.Serve(httpListener) }()
	log.Info("backend ready", "http", cfg.HTTPAddress, "grpc", cfg.GRPCAddress)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-results:
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	workerStop()
	select {
	case <-configDone:
	case <-shutdown.Done():
	}
	select {
	case <-workerDone:
	case <-shutdown.Done():
	}
	select {
	case <-collectorDone:
	case <-shutdown.Done():
	}
	select {
	case <-correlationDone:
	case <-shutdown.Done():
	}
	select {
	case <-aiDone:
	case <-shutdown.Done():
	}
	select {
	case <-reportDone:
	case <-shutdown.Done():
	}
	select {
	case <-sourceDone:
	case <-shutdown.Done():
	}
	select {
	case <-topologyDone:
	case <-shutdown.Done():
	}
	defer done()
	grpcDone := make(chan struct{})
	go func() { g.GracefulStop(); close(grpcDone) }()
	httpErr := h.Shutdown(shutdown)
	if enrollmentServer != nil {
		if err := enrollmentServer.Shutdown(shutdown); err != nil {
			_ = enrollmentServer.Close()
		}
	}
	if httpErr != nil {
		_ = h.Close()
	}
	select {
	case <-grpcDone:
	case <-shutdown.Done():
		g.Stop()
		<-grpcDone
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return serveErr
	}
	log.Info("backend shutdown complete")
	return nil
}

func serverTLS(cfg config.Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS credentials: %w", err)
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if cfg.TLSClientCA != "" {
		b, err := os.ReadFile(cfg.TLSClientCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("client CA file has no certificates")
		}
		c.ClientCAs = pool
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, nil
}
