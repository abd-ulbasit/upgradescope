package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// certCheckInterval is how often a handshake may stat the key pair's
// files for a change: at most one pair of stats a second, whatever the
// handshake rate.
const certCheckInterval = time.Second

// keyPair serves the certificate in certFile and keyFile and re-reads the
// pair when either file changes, so a renewal (cert-manager rewriting the
// mounted Secret, which the kubelet swaps in atomically) reaches new
// connections without a restart. A pair that does not load — a key not yet
// in step with its certificate, a truncated file — is skipped and the
// previous certificate stays in service; the next check tries again.
type keyPair struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   [2]os.FileInfo // of the files cert was loaded from
	checked time.Time      // last stat
	failure string         // last reload error logged, "" = none since the last load
}

// newKeyPair loads the pair now, so a bad one fails startup instead of the
// first handshake.
func newKeyPair(certFile, keyFile string) (*keyPair, error) {
	k := &keyPair{certFile: certFile, keyFile: keyFile, checked: time.Now()}
	stamp, err := k.stat()
	if err != nil {
		return nil, err
	}
	if err := k.load(stamp); err != nil {
		return nil, err
	}
	return k, nil
}

// getCertificate is tls.Config.GetCertificate.
func (k *keyPair) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if now := time.Now(); now.Sub(k.checked) >= certCheckInterval {
		k.checked = now
		k.reloadIfChanged()
	}
	return k.cert, nil
}

func (k *keyPair) reloadIfChanged() {
	stamp, err := k.stat()
	if err == nil && sameFile(stamp[0], k.stamp[0]) && sameFile(stamp[1], k.stamp[1]) {
		return
	}
	if err == nil {
		err = k.load(stamp)
	}
	if err != nil {
		// Logged once per distinct error, not on every check.
		if msg := err.Error(); msg != k.failure {
			k.failure = msg
			log.Printf("WARN server: reloading the TLS key pair: %v; still serving the previous certificate", err)
		}
		return
	}
	log.Printf("server: TLS key pair reloaded from %s and %s", k.certFile, k.keyFile)
}

// stat stats both files, following symlinks: a Secret volume's files are
// symlinks through ..data, which the kubelet repoints on an update.
func (k *keyPair) stat() ([2]os.FileInfo, error) {
	var stamp [2]os.FileInfo
	for i, f := range []string{k.certFile, k.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return stamp, fmt.Errorf("server: TLS key pair: %w", err)
		}
		stamp[i] = fi
	}
	return stamp, nil
}

// load reads the pair; stamp, taken before the read, is what the next
// check compares with. A file changed between the stat and the read is
// read again on the next check, which is harmless.
func (k *keyPair) load(stamp [2]os.FileInfo) error {
	cert, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		return fmt.Errorf("server: load TLS key pair: %w", err)
	}
	k.cert, k.stamp, k.failure = &cert, stamp, ""
	return nil
}

// sameFile reports whether b is a's file, unchanged: the same file (a
// replaced file is another one), modification time and size.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}
