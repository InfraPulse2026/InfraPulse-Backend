package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// InitCA creates a NEW dedicated directory; existing directories are never
// modified. Deployments must restrict the issuing key with host ACLs/secrets.
func InitCA(directory string, hosts []string) error {
	if len(hosts) == 0 || len(hosts) > 32 {
		return errors.New("1..32 server hosts required")
	}
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial := func() (*big.Int, error) {
		n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
		if e == nil && n.Sign() == 0 {
			n.SetInt64(1)
		}
		return n, e
	}
	caSerial, err := serial()
	if err != nil {
		return err
	}
	serverSerial, err := serial()
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "InfraPulse issuing CA"}, IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0)}
	server := &x509.Certificate{SerialNumber: serverSerial, Subject: pkix.Name{CommonName: "InfraPulse server"}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0)}
	hostname := regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)
	for _, host := range hosts {
		if host != strings.TrimSpace(host) || host == "" || len(host) > 253 {
			return errors.New("invalid server host")
		}
		if ip := net.ParseIP(host); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else if hostname.MatchString(host) && !strings.Contains(host, "..") {
			server.DNSNames = append(server.DNSNames, host)
		} else {
			return errors.New("server hosts must be DNS names or IP addresses")
		}
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	parsedCA, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, parsedCA, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return err
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return err
	}
	material := []struct {
		name, kind string
		data       []byte
	}{{"ca.pem", "CERTIFICATE", caDER}, {"ca.key", "PRIVATE KEY", caKeyDER}, {"server.pem", "CERTIFICATE", serverDER}, {"server.key", "PRIVATE KEY", serverKeyDER}}
	if err = os.Mkdir(directory, 0700); err != nil {
		return errors.New("CA directory must be new and its parent must already exist")
	}
	created := []string{}
	success := false
	defer func() {
		if !success {
			for _, path := range created {
				_ = os.Remove(path)
			}
			_ = os.Remove(directory)
		}
	}()
	for _, m := range material {
		path := filepath.Join(directory, m.name)
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		created = append(created, path)
		e = pem.Encode(f, &pem.Block{Type: m.kind, Bytes: m.data})
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	success = true
	return nil
}
