package server

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSecretVolume lays out certPEM and keyPEM as the kubelet does for a
// mounted kubernetes.io/tls Secret: the files live in a timestamped
// directory, ..data points at it, and tls.crt and tls.key point through
// ..data. Called again on the same dir it swaps ..data atomically, which is
// how a renewed Secret reaches the pod.
func writeSecretVolume(t *testing.T, dir, version string, certPEM, keyPEM []byte) {
	t.Helper()
	ts := filepath.Join(dir, "..ts_"+version)
	if err := os.Mkdir(ts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(ts), tmp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// servedSerial handshakes with addr on a fresh connection, trusting only
// pool, and returns the serial number of the leaf certificate it got.
func servedSerial(t *testing.T, addr string, pool *x509.CertPool) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("handshake with %s: %v", addr, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// waitForSerial handshakes until the server presents the certificate with
// serial want, failing after 10s.
func waitForSerial(t *testing.T, addr string, pool *x509.CertPool, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := servedSerial(t, addr, pool)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server still presents certificate %d, want %d after rotating the files", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cert-manager renews the Secret, the kubelet swaps the mounted files, and
// the server presents the new certificate on new connections without a
// restart. A half-written pair (a new certificate with the old key) is not
// loaded: the previous certificate stays in service until the pair matches.
func TestServeTLSReloadsRotatedKeyPair(t *testing.T) {
	cert1PEM, key1PEM, cert1 := selfSignedPEM(t, 1)
	cert2PEM, key2PEM, cert2 := selfSignedPEM(t, 2)
	cert3PEM, key3PEM, cert3 := selfSignedPEM(t, 3)
	pool := x509.NewCertPool()
	for _, c := range []*x509.Certificate{cert1, cert2, cert3} {
		pool.AddCert(c)
	}
	dir := t.TempDir()
	writeSecretVolume(t, dir, "1", cert1PEM, key1PEM)
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")

	captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "127.0.0.1:0"
		c.TLSCertFile = certFile
		c.TLSKeyFile = keyFile
	})
	startServer(t, s)
	if got := servedSerial(t, s.Addr(), pool); got != 1 {
		t.Fatalf("served certificate %d, want 1", got)
	}

	writeSecretVolume(t, dir, "2", cert2PEM, key2PEM)
	waitForSerial(t, s.Addr(), pool, 2)

	// Out of step: a new certificate in place, its key not yet.
	realCert := filepath.Join(dir, "..ts_2", "tls.crt")
	if err := os.WriteFile(realCert, cert3PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * certCheckInterval)
	if got := servedSerial(t, s.Addr(), pool); got != 2 {
		t.Fatalf("with a mismatched pair on disk the server presents %d, want the previous certificate 2", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "..ts_2", "tls.key"), key3PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSerial(t, s.Addr(), pool, 3)
}

// The TLS listener offers TLS 1.2 at the oldest: a client capped at 1.1
// is refused.
func TestServeTLSMinimumVersion(t *testing.T) {
	certFile, keyFile, cert := writeSelfSignedCert(t)
	captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "127.0.0.1:0"
		c.TLSCertFile = certFile
		c.TLSKeyFile = keyFile
	})
	startServer(t, s)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	conn, err := tls.Dial("tcp", s.Addr(), &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11})
	if err == nil {
		conn.Close()
		t.Fatal("TLS 1.1 handshake succeeded, want it refused")
	}
}
