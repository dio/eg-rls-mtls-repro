package repro

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dio/egtest"
	common "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	rls "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

//go:embed testdata/helm-values.yaml
var values string

//go:embed testdata/redis.yaml
var redis []byte

const namespace = "envoy-gateway-system"

func TestClientAuthentication(t *testing.T) {
	if os.Getenv("RUN_EG") != "1" {
		t.Skip("set RUN_EG=1 to create disposable clusters")
	}
	for _, fixed := range []bool{false, true} {
		name := "generated"
		helm := values
		if fixed {
			name = "corrected"
			helm = strings.Replace(values, "env: []", "env:\n              - name: GRPC_CLIENT_TLS_CACERT\n                value: /certs/ca.crt", 1)
		}
		t.Run(name, func(t *testing.T) {
			cluster := egtest.New(t, egtest.Options{EGVersion: "v1.9.1", K3SVersion: "v1.33.13-k3s2", HelmValues: []byte(helm)})
			t.Cleanup(func() {
				// Close is idempotent; record evidence after removal, before New's fallback cleanup.
				if err := cluster.Close(); err != nil {
					t.Error(err)
				}
				t.Logf("cluster cleanup: %s", cluster.Info().Cleanup)
			})
			if err := cluster.Apply(t.Context(), redis); err != nil {
				t.Fatal(err)
			}
			if err := cluster.WaitDeployment(t.Context(), namespace, "redis"); err != nil {
				t.Fatal(err)
			}
			if err := cluster.WaitDeployment(t.Context(), namespace, "envoy-ratelimit"); err != nil {
				t.Fatal(err)
			}
			// Assert the generated input too: this must not silently become a different repro.
			raw, err := cluster.Kubectl(t.Context(), nil, "-n", namespace, "get", "deployment", "envoy-ratelimit", "-o", "json")
			if err != nil {
				t.Fatal(err)
			}
			var deployment struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Env []struct{ Name, Value string }
							}
						}
					}
				}
			}
			if err := json.Unmarshal(raw, &deployment); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, c := range deployment.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					env[e.Name] = e.Value
				}
			}
			if env["GRPC_SERVER_TLS_CA_CERT"] != "/certs/ca.crt" || (env["GRPC_CLIENT_TLS_CACERT"] == "/certs/ca.crt") != fixed {
				t.Fatal("unexpected generated client CA environment")
			}
			certData := secret(t, cluster, "envoy")
			trusted, err := tls.X509KeyPair(certData["tls.crt"], certData["tls.key"])
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(certData["ca.crt"]) {
				t.Fatal("invalid server CA")
			}
			foreign := foreignCertificate(t)
			for _, client := range []struct {
				name string
				cert *tls.Certificate
			}{{"trusted", &trusted}, {"none", nil}, {"unrelated", &foreign}, {"trusted_after", &trusted}} {
				t.Run(client.name, func(t *testing.T) {
					forward, err := cluster.PortForward(t.Context(), namespace, "service/envoy-ratelimit", 8081)
					if err != nil {
						t.Fatal(err)
					}
					defer forward.Close()
					endpoint, err := url.Parse(forward.URL)
					if err != nil {
						t.Fatal(err)
					}

					cfg := &tls.Config{RootCAs: roots, ServerName: "envoy-ratelimit." + namespace, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
					// Force the unrelated certificate to be offered even when the server's CA
					// list does not match it; otherwise this only tests an omitted certificate.
					if client.cert != nil {
						cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return client.cert, nil }
					}
					conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					_, err = rls.NewRateLimitServiceClient(conn).ShouldRateLimit(ctx, &rls.RateLimitRequest{Domain: "repro", Descriptors: []*common.RateLimitDescriptor{{Entries: []*common.RateLimitDescriptor_Entry{{Key: "test", Value: "client-auth"}}}}})
					reject := fixed && (client.name == "none" || client.name == "unrelated")
					if reject {
						if err == nil {
							t.Fatal("unauthenticated RPC unexpectedly accepted")
						}
						// TLS 1.2 reports a missing client certificate as handshake_failure;
						// an offered untrusted certificate reports unknown_ca.
						alert := "remote error: tls: unknown certificate authority"
						if client.name == "none" {
							alert = "remote error: tls: handshake failure"
						}
						if !strings.Contains(err.Error(), alert) {
							t.Fatalf("expected certificate rejection, got %v", err)
						}
						t.Logf("RPC rejected: %v", err)
					} else {
						if status.Code(err) != codes.Unknown || status.Convert(err).Message() != "no rate limit configuration loaded" {
							t.Fatalf("expected RLS application response, got %v", err)
						}
						t.Log("RPC reached RLS: no rate limit configuration loaded")
					}
				})
			}
		})
	}
}

func secret(t *testing.T, cluster *egtest.Cluster, name string) map[string][]byte {
	t.Helper()
	raw, err := cluster.Kubectl(t.Context(), nil, "-n", namespace, "get", "secret", name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct{ Data map[string][]byte }
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Data
}

func foreignCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "unrelated client"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
