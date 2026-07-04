package security

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testPKI struct {
	caFile         string
	serverCert     string
	serverKey      string
	clientCert     string
	clientKey      string
	foreignCert    string
	foreignCertKey string
}

func generatePKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, caCert := generateCA(t, "otus-test-ca")
	caFile := writePEM(t, dir, "ca.crt", "CERTIFICATE", caCert.Raw)

	serverCert, serverKey := issueCert(t, dir, "server", caCert, caKey)
	clientCert, clientKey := issueCert(t, dir, "client", caCert, caKey)

	foreignKey, foreignCA := generateCA(t, "чужой-ca")
	foreignCert, foreignCertKey := issueCert(t, dir, "foreign", foreignCA, foreignKey)

	return testPKI{
		caFile:         caFile,
		serverCert:     serverCert,
		serverKey:      serverKey,
		clientCert:     clientCert,
		clientKey:      clientKey,
		foreignCert:    foreignCert,
		foreignCertKey: foreignCertKey,
	}
}

func generateCA(t *testing.T, name string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ CA: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("не удалось создать сертификат CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("не удалось разобрать сертификат CA: %v", err)
	}
	return key, cert
}

func issueCert(t *testing.T, dir, name string, ca *x509.Certificate, caKey *rsa.PrivateKey) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ %s: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("не удалось выписать сертификат %s: %v", name, err)
	}
	certFile := writePEM(t, dir, name+".crt", "CERTIFICATE", der)
	keyFile := writePEM(t, dir, name+".key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	return certFile, keyFile
}

func writePEM(t *testing.T, dir, name, blockType string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("не удалось записать %s: %v", name, err)
	}
	return path
}

func startMTLSServer(t *testing.T, pki testPKI) *httptest.Server {
	t.Helper()
	t.Setenv("TLS_CERT_FILE", pki.serverCert)
	t.Setenv("TLS_KEY_FILE", pki.serverKey)
	t.Setenv("TLS_CA_FILE", pki.caFile)
	serverCfg, err := ServerTLS()
	if err != nil {
		t.Fatalf("не удалось собрать серверный TLS: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = serverCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestMTLSHandshake(t *testing.T) {
	pki := generatePKI(t)
	srv := startMTLSServer(t, pki)

	t.Setenv("TLS_CERT_FILE", pki.clientCert)
	t.Setenv("TLS_KEY_FILE", pki.clientKey)
	t.Setenv("TLS_CA_FILE", pki.caFile)
	clientCfg, err := ClientTLS()
	if err != nil {
		t.Fatalf("не удалось собрать клиентский TLS: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS-запрос не прошёл: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("ожидался статус 200, получен %d", resp.StatusCode)
	}
}

func TestMTLSRejectsClientWithoutCert(t *testing.T) {
	pki := generatePKI(t)
	srv := startMTLSServer(t, pki)

	t.Setenv("TLS_CERT_FILE", "")
	t.Setenv("TLS_KEY_FILE", "")
	t.Setenv("TLS_CA_FILE", pki.caFile)
	clientCfg, err := ClientTLS()
	if err != nil {
		t.Fatalf("не удалось собрать клиентский TLS: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("сервер принял клиента без сертификата")
	}
}

func TestMTLSRejectsForeignCert(t *testing.T) {
	pki := generatePKI(t)
	srv := startMTLSServer(t, pki)

	t.Setenv("TLS_CERT_FILE", pki.foreignCert)
	t.Setenv("TLS_KEY_FILE", pki.foreignCertKey)
	t.Setenv("TLS_CA_FILE", pki.caFile)
	clientCfg, err := ClientTLS()
	if err != nil {
		t.Fatalf("не удалось собрать клиентский TLS: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("сервер принял сертификат, подписанный чужим CA")
	}
}

func TestTLSDisabledWithoutEnv(t *testing.T) {
	t.Setenv("TLS_CERT_FILE", "")
	t.Setenv("TLS_KEY_FILE", "")
	t.Setenv("TLS_CA_FILE", "")
	serverCfg, err := ServerTLS()
	if err != nil || serverCfg != nil {
		t.Errorf("без переменных окружения TLS должен быть выключен: cfg=%v err=%v", serverCfg, err)
	}
	clientCfg, err := ClientTLS()
	if err != nil || clientCfg != nil {
		t.Errorf("без переменных окружения TLS должен быть выключен: cfg=%v err=%v", clientCfg, err)
	}
}
