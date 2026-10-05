// Command providersim is the deterministic HTTPS provider simulator used by
// the enrichment benchmark (benchmarks/run_enrich.py).
//
// It answers every request with {"data":{"email":"info@<domain>"}} after a
// fixed latency, where <domain> comes from the `domain` query parameter or the
// JSON body. It serves TLS with a throwaway self-signed certificate for
// 127.0.0.1 (manifest v1 connectors must be https), writes the certificate to
// -cert-out so the clients can trust it through SSL_CERT_FILE, and prints one
// line, "READY <base-url>", when it is listening. GET /_stats reports the
// number of requests served and the highest number in flight.
//
// Standard library only, so it never becomes the bottleneck it is measuring.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"
)

func main() {
	latency := flag.Duration("latency", 5*time.Millisecond, "fixed response latency")
	certOut := flag.String("cert-out", "", "write the PEM certificate here (required)")
	flag.Parse()
	if *certOut == "" {
		fmt.Fprintln(os.Stderr, "providersim: -cert-out is required")
		os.Exit(2)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "opengtm-providersim"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	check(err)
	check(os.WriteFile(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))

	var served, inflight, maxInflight atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/_stats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"served": served.Load(), "max_inflight": maxInflight.Load()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := maxInflight.Load()
			if n <= m || maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		domain := r.URL.Query().Get("domain")
		if domain == "" && r.Body != nil {
			var body struct {
				Domain string `json:"domain"`
			}
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			_ = json.Unmarshal(raw, &body)
			domain = body.Domain
		}
		time.Sleep(*latency)
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"email":"info@%s"}}`, domain)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	fmt.Printf("READY https://%s\n", ln.Addr())
	os.Stdout.Sync() //nolint:errcheck

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		_ = srv.Close()
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
		check(err)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "providersim:", err)
		os.Exit(1)
	}
}
