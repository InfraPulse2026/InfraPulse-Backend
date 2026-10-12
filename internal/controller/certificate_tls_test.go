package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestRealMTLSCertificateCannotImpersonateAnotherAgent(t *testing.T) {
	const id = "bf6e7560-9b04-4d28-ae98-5dfe7d4f2001"
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyPEM := func(key *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverPair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), keyPEM(serverKey))
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := pki.AgentURI(id)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{uri}}, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := pki.NewIssuer(caPEM, keyPEM(caKey), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := issuer.Sign(id, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), now)
	if err != nil {
		t.Fatal(err)
	}
	clientPair, err := tls.X509KeyPair(issued.Certificate, keyPEM(clientKey))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	repo := &memoryRepository{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverPair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})))
	pb.RegisterAgentServiceServer(server, New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAgentAuthorizer(pki.AuthorizePeer)))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "localhost", Certificates: []tls.Certificate{clientPair}})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := pb.NewAgentServiceClient(conn)
	// A trusted certificate and successful TLS handshake must not grant another ID.
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forged := testHello()
	forged.AgentId = "bf6e7560-9b04-4d28-ae98-5dfe7d4f2002"
	forged.GetHello().AgentId = forged.AgentId
	if err = stream.Send(forged); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("impersonation was not rejected: %v", err)
	}
	repo.mu.Lock()
	count := len(repo.messages)
	repo.mu.Unlock()
	if count != 0 {
		t.Fatal("forged identity registered")
	}
	stream, err = client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := testHello()
	hello.AgentId = id
	hello.GetHello().AgentId = id
	if err = stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	if ack, err := stream.Recv(); err != nil || !ack.GetHelloAck().GetAccepted() {
		t.Fatalf("legitimate certificate rejected: %v", err)
	}
	if err = stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("unexpected close: %v", err)
	}
}
