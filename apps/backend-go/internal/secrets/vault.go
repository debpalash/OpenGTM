package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const vaultTimeout = 10 * time.Second

// vaultClient talks to HashiCorp Vault Transit. The address is operator
// configured and trusted, so it deliberately uses a plain http.Client rather
// than the SSRF-guarded egress client. Error strings never carry response
// bodies, tokens or URLs.
type vaultClient struct {
	cfg Config
	hc  *http.Client
}

func newVaultClient(cfg Config, hc *http.Client) *vaultClient {
	return &vaultClient{cfg: cfg, hc: hc}
}

func (v *vaultClient) client() (*http.Client, error) {
	if v.hc != nil {
		return v.hc, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ca := v.cfg.VaultCACert; ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, errors.New("Could not read VAULT_CACERT")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("VAULT_CACERT contains no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// config validates the address and resolves the token like _vault_config.
func (v *vaultClient) config() (addr, token string, err error) {
	addr = strings.TrimRight(strings.TrimSpace(v.cfg.VaultAddr), "/")
	u, perr := url.Parse(addr)
	if perr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(addr, "#") {
		return "", "", errors.New("VAULT_ADDR must be a clean HTTPS URL")
	}
	token = strings.TrimSpace(v.cfg.VaultToken)
	if f := strings.TrimSpace(v.cfg.VaultTokenFile); f != "" {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			return "", "", errors.New("Could not read VAULT_TOKEN_FILE")
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" || strings.TrimSpace(v.cfg.VaultTransitKey) == "" {
		return "", "", errors.New("Vault Transit requires a token and VAULT_TRANSIT_KEY")
	}
	return addr, token, nil
}

// request performs a transit call and returns data.<ciphertext|plaintext>.
func (v *vaultClient) request(ctx context.Context, action string, payload map[string]string) (string, error) {
	addr, token, err := v.config()
	if err != nil {
		return "", err
	}
	hc, err := v.client()
	if err != nil {
		return "", err
	}
	failed := fmt.Errorf("Vault Transit %s failed", action)
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, vaultTimeout)
	defer cancel()
	// PathEscape encodes "/" and spaces like Python quote(safe="").
	endpoint := addr + "/v1/transit/" + action + "/" + url.PathEscape(v.cfg.VaultTransitKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", failed
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")
	if ns := v.cfg.VaultNamespace; ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", failed
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", failed
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", failed
	}
	field := "plaintext"
	if action == "encrypt" {
		field = "ciphertext"
	}
	s, ok := out.Data[field].(string)
	if !ok || s == "" {
		return "", fmt.Errorf("Vault Transit %s returned no %s", action, field)
	}
	return s, nil
}
