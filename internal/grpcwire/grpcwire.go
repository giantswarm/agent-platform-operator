// Package grpcwire calls the few gRPC methods the erasure needs on kagent and
// Substrate with hand-encoded protobuf messages, as the operator's own
// ServiceAccount.
//
// The operator calls three methods and reads a handful of fields; the
// generated clients of kagent and Substrate come with their modules, which
// need Go 1.27.3 (this repository is on 1.26). The field numbers are those of
// the services' .proto files, named in each caller. Exit: once this
// repository's Go reaches the modules' go directive, replace the callers with
// the generated clients (kagent's go/api/gen, Substrate's
// pkg/proto/ateapipb) and remove this package.
package grpcwire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
)

// Target schemes: plaintext h2c (an in-cluster Service) and TLS.
const (
	SchemePlaintext = "grpc"
	SchemeTLS       = "grpcs"
)

// Config is how a Conn reaches one server.
type Config struct {
	// Target is grpc://host:port or grpcs://host[:port] (443 by default).
	Target string
	// CAFile verifies a grpcs server; empty uses the system roots. It is
	// read again at each handshake, so a rotated bundle is trusted on the
	// next connection.
	CAFile string
	// TokenFile is the projected ServiceAccount token sent as the bearer of
	// every call, read at each call: the kubelet rotates it.
	TokenFile string
}

// Conn calls methods of one server.
type Conn struct {
	conn      *grpc.ClientConn
	tokenFile string
}

// Dial builds a Conn; it connects on the first call.
func Dial(cfg Config) (*Conn, error) {
	hostPort, useTLS, err := parseTarget(cfg.Target)
	if err != nil {
		return nil, err
	}
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("%s: no token file", cfg.Target)
	}
	creds := insecure.NewCredentials()
	if useTLS {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		creds = credentials.NewTLS(tlsConfig)
		if cfg.CAFile != "" {
			if _, err := readRoots(cfg.CAFile); err != nil {
				return nil, fmt.Errorf("%s: %w", cfg.Target, err)
			}
			creds = &fileRootsCreds{TransportCredentials: creds, base: tlsConfig, caFile: cfg.CAFile}
		}
	}
	conn, err := grpc.NewClient(hostPort, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Target, err)
	}
	return &Conn{conn: conn, tokenFile: cfg.TokenFile}, nil
}

func readRoots(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(filepath.Clean(caFile))
	if err != nil {
		return nil, fmt.Errorf("read the CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the CA file %s holds no certificate", caFile)
	}
	return roots, nil
}

// fileRootsCreds verifies each new connection against the CA file as it is
// at that handshake.
type fileRootsCreds struct {
	credentials.TransportCredentials
	base   *tls.Config
	caFile string
}

func (c *fileRootsCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	roots, err := readRoots(c.caFile)
	if err != nil {
		return nil, nil, err
	}
	tlsConfig := c.base.Clone()
	tlsConfig.RootCAs = roots
	return credentials.NewTLS(tlsConfig).ClientHandshake(ctx, authority, rawConn)
}

func (c *fileRootsCreds) Clone() credentials.TransportCredentials {
	return &fileRootsCreds{TransportCredentials: c.TransportCredentials.Clone(), base: c.base.Clone(), caFile: c.caFile}
}

func (c *fileRootsCreds) OverrideServerName(name string) error {
	c.base.ServerName = name
	return nil
}

// NewConn is a Conn on an existing connection.
func NewConn(conn *grpc.ClientConn, tokenFile string) *Conn {
	return &Conn{conn: conn, tokenFile: tokenFile}
}

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// CallTimeout bounds one call, so a server that stops answering never holds
// the reconciler.
const CallTimeout = 30 * time.Second

// Invoke calls method with the encoded request and returns the encoded
// response.
func (c *Conn) Invoke(ctx context.Context, method string, req []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the ServiceAccount token: %w", err)
	}
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+strings.TrimSpace(string(token))))
	var resp []byte
	if err := c.conn.Invoke(ctx, method, &req, &resp, grpc.ForceCodecV2(codec{})); err != nil {
		return nil, err
	}
	return resp, nil
}

func parseTarget(target string) (hostPort string, useTLS bool, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", false, fmt.Errorf("target %q: %w", target, err)
	}
	switch u.Scheme {
	case SchemePlaintext:
	case SchemeTLS:
		useTLS = true
	default:
		return "", false, fmt.Errorf("target %q must use the %s:// or %s:// scheme", target, SchemePlaintext, SchemeTLS)
	}
	if u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("target %q must be %s://host[:port] with no path", target, u.Scheme)
	}
	if u.Port() != "" {
		return u.Host, useTLS, nil
	}
	if !useTLS {
		return "", false, fmt.Errorf("target %q must name a port", target)
	}
	return net.JoinHostPort(u.Hostname(), "443"), true, nil
}

// codec passes encoded messages through as they are, under the name of the
// protobuf codec, so the server reads them with its own.
type codec struct{}

func (codec) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("grpcwire: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(*b)}, nil
}

func (codec) Unmarshal(data mem.BufferSlice, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("grpcwire: cannot unmarshal into %T", v)
	}
	*b = data.Materialize()
	return nil
}

func (codec) Name() string { return "proto" }

// ---- encoding ---------------------------------------------------------------

// Message builds an encoded protobuf message.
type Message []byte

// String appends a string field; an empty one is left out, as proto3 does.
func (m Message) String(num protowire.Number, v string) Message {
	if v == "" {
		return m
	}
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendString(m, v)
}

// Bool appends a bool field; false is left out.
func (m Message) Bool(num protowire.Number, v bool) Message {
	if !v {
		return m
	}
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, 1)
}

// Uint appends a non-negative integer field (int32 and the like); 0 is left
// out.
func (m Message) Uint(num protowire.Number, v uint64) Message {
	if v == 0 {
		return m
	}
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, v)
}

// Message appends a message field.
func (m Message) Message(num protowire.Number, v Message) Message {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendBytes(m, v)
}

// ---- decoding ---------------------------------------------------------------

// Fields are the length-delimited fields of an encoded message (strings and
// messages) by number, in order; fields of other wire types are skipped.
func Fields(b []byte) (map[protowire.Number][][]byte, error) {
	out := map[protowire.Number][][]byte{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		if typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			out[num] = append(out[num], v)
			b = b[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
	}
	return out, nil
}

// First is the first value of field num, or nil.
func First(fields map[protowire.Number][][]byte, num protowire.Number) []byte {
	if v := fields[num]; len(v) > 0 {
		return v[0]
	}
	return nil
}

// ErrPageLimit is returned when a listing has more pages than its caller
// follows.
var ErrPageLimit = errors.New("the listing has more pages than the operator follows")
