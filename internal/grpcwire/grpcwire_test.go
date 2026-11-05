package grpcwire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca testCA) serving(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ate-api"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// writeAtomically replaces path the way the kubelet updates a projected
// volume: a new file swapped in, never an in-place write.
func writeAtomically(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".new"
	require.NoError(t, os.WriteFile(tmp, data, 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func serveTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
		grpc.ForceServerCodecV2(codec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			var req []byte
			if err := stream.RecvMsg(&req); err != nil {
				return err
			}
			return stream.SendMsg(&req)
		}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestDialTrustsARotatedCAFileWithoutARedial(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "trust-bundle.pem")
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("token"), 0o600))

	oldCA, newCA := newTestCA(t, "old"), newTestCA(t, "new")
	writeAtomically(t, caFile, oldCA.pem)
	addr := serveTLS(t, newCA.serving(t))

	conn, err := Dial(Config{Target: "grpcs://" + addr, CAFile: caFile, TokenFile: tokenFile})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Invoke(t.Context(), "/test.Echo/Echo", []byte("x"))
	require.ErrorContains(t, err, "certificate signed by unknown authority")

	writeAtomically(t, caFile, newCA.pem)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		resp, err := conn.Invoke(t.Context(), "/test.Echo/Echo", []byte("x"))
		require.NoError(c, err)
		require.Equal(c, []byte("x"), resp)
	}, 15*time.Second, 100*time.Millisecond)
}

func TestDialRefusesACAFileWithoutACertificate(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "trust-bundle.pem")
	require.NoError(t, os.WriteFile(caFile, nil, 0o600))
	_, err := Dial(Config{Target: "grpcs://ate-api:443", CAFile: caFile, TokenFile: filepath.Join(dir, "token")})
	require.ErrorContains(t, err, "holds no certificate")
}
