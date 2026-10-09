# Envoy Gateway / RLS client certificate reproducer

[![test](https://github.com/dio/eg-rls-mtls-repro/actions/workflows/test.yaml/badge.svg)](https://github.com/dio/eg-rls-mtls-repro/actions/workflows/test.yaml)

A small Go integration test using [egtest](https://github.com/dio/egtest) to
compare client authentication on an Envoy Gateway-managed rate-limit service.
It creates two disposable k3d clusters, one per configuration, and removes them
through test cleanup. No existing Kubernetes context is used.

## Run

Requires Go 1.27.1+, Docker running, k3d, kubectl and Helm on PATH.

```sh
RUN_EG=1 go test -v -count=1 -timeout=25m ./...
```

`go test ./...` without `RUN_EG=1` only compiles and skips the live test.

Pins: Envoy Gateway **v1.9.1**, k3s **v1.33.13-k3s2**, upstream
`envoyproxy/ratelimit:8fe6ea42` (multi-platform digest pinned in the Helm fixture),
and Redis **7.4.2-alpine**. No custom image builds or application credentials.

## What it demonstrates

EG supplies `GRPC_SERVER_TLS_CA_CERT=/certs/ca.crt`. This RLS revision reads
`GRPC_CLIENT_TLS_CACERT` to configure validation of incoming client certificates.
With the recognized variable unset, its gRPC server uses `tls.NoClientCert`.
TLS still encrypts the connection and clients still validate the server.

The second cluster adds only this deployment environment entry:

```yaml
config:
  envoyGateway:
    provider:
      kubernetes:
        rateLimitDeployment:
          container:
            env:
              - name: GRPC_CLIENT_TLS_CACERT
                value: /certs/ca.crt
```

Expected results (a passing test confirms this matrix):

| gRPC client | EG-generated configuration | With recognized client CA variable |
| --- | --- | --- |
| EG-issued certificate | RPC accepted | RPC accepted |
| No certificate | RPC accepted | Certificate rejection |
| Certificate signed by unrelated CA | RPC accepted | Certificate rejection |
| EG-issued certificate, checked again | RPC accepted | RPC accepted |

Each probe calls `ShouldRateLimit` over a new TLS connection to the real RLS
service through a separate private port-forward per probe. Probes use TLS 1.2
so client-certificate rejection is observed during the handshake. Server CA and
hostname verification stay enabled. The unrelated client is forced to offer its certificate when requested,
even if it does not match the server's advertised CA list. Under the generated
configuration, the server does not request a client certificate at all.

The test uses an unconfigured domain (`repro`); the exact gRPC application
error `Unknown: no rate limit configuration loaded` demonstrates that the RPC
reached RLS. Transport errors do not count as acceptance. It intentionally installs no Gateway,
HTTPRoute, or rate-limit policy. It tests the RLS client-authentication boundary,
not Envoy request forwarding, quota enforcement, or accounting. Test certificates
are ephemeral; private keys are neither printed nor committed.

## Upstream source

- [EG v1.9.1 generated RLS environment](https://github.com/envoyproxy/gateway/blob/v1.9.1/internal/infrastructure/kubernetes/ratelimit/resource.go)
- [RLS settings and client-authentication selection](https://github.com/envoyproxy/ratelimit/blob/8fe6ea421048bdb2a8873d5f676b3f6eac76a997/src/settings/settings.go)

## Validation status

Locally checked on macOS/arm64: compilation, `go vet`, and all four generated-
configuration probes passed on the committed test. Earlier live runs also
observed missing/untrusted client rejection and trusted-client access with the
correction. After refining the TLS-alert assertions, the final corrected-case
reruns timed out pulling the Envoy Gateway image during setup. A completely
passing run of the final suite has therefore not yet been recorded.

GitHub Actions runs compilation/vet and a separate Linux live job on every push,
pull request, and manual dispatch. The live job checks Docker Hub connectivity
with up to three image-pull attempts before running the test once. Assertion
failures are not automatically retried. The `live-result` artifact contains the
test log and a post-run resource cleanup check; use the linked run for current
results. The preflight image is pulled into the host Docker daemon, while k3d
nodes have separate containerd stores and still pull their own images.
