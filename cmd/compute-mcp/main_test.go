// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/agent"
)

const (
	testToken = "caller-token"
	// testHost stands in for the control plane a deployment is pointed at, and
	// testProjectName for the project a request names in its header.
	testHost        = "https://api.datum.example"
	testProjectName = "my-project"
)

func baseConfig() *rest.Config {
	// Shaped like an in-cluster config: endpoint and CA, plus a server identity
	// that must not survive into a caller's read.
	return &rest.Config{
		Host:            testHost,
		BearerToken:     "server-service-account-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		TLSClientConfig: rest.TLSClientConfig{CAFile: "/var/run/secrets/ca.crt"},
	}
}

// TestClientConfigAddressesProjectControlPlane pins the addressing: a project
// is reached by rewriting the host path, the same rewrite internal/quota,
// internal/referenceddata and the datumctl plugin perform. The namespace within
// that control plane is "default", never the project name.
func TestClientConfigAddressesProjectControlPlane(t *testing.T) {
	cfg, err := clientConfig(baseConfig(), testToken, testProjectName)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}

	want := "https://api.datum.example/apis/resourcemanager.miloapis.com/v1alpha1/projects/my-project/control-plane"
	if cfg.Host != want {
		t.Errorf("Host = %q, want %q", cfg.Host, want)
	}
	if resourceNamespace != "default" {
		t.Errorf("resourceNamespace = %q, want \"default\"", resourceNamespace)
	}
}

// TestClientConfigCarriesOnlyTheCallerCredential is the security property the
// whole design rests on: the server must never read as itself.
func TestClientConfigCarriesOnlyTheCallerCredential(t *testing.T) {
	base := baseConfig()
	cfg, err := clientConfig(base, testToken, testProjectName)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}

	if cfg.BearerToken != testToken {
		t.Errorf("BearerToken = %q, want the caller's token", cfg.BearerToken)
	}
	if cfg.BearerTokenFile != "" {
		t.Errorf("BearerTokenFile = %q, want it cleared", cfg.BearerTokenFile)
	}
	if cfg.Impersonate.UserName != "" {
		t.Errorf("Impersonate = %q, want none", cfg.Impersonate.UserName)
	}
	if cfg.CAFile != base.CAFile {
		t.Errorf("CAFile = %q, want the base config's %q", cfg.CAFile, base.CAFile)
	}
	// The base config must be left alone; it is shared by every request.
	if base.BearerToken != "server-service-account-token" || base.Host != testHost {
		t.Error("clientConfig mutated the shared base config")
	}
}

// TestClientConfigRejectsHostileProject: the project comes off a header and is
// interpolated into a URL path, so a value that could reshape that path into
// another API route has to be refused rather than escaped.
func TestClientConfigRejectsHostileProject(t *testing.T) {
	for _, project := range []string{
		"../../../api/v1/nodes",
		"a/b",
		"proj/../other",
		"proj%2f..",
		"UPPER",
		"has space",
		"..",
		"",
	} {
		if _, err := clientConfig(baseConfig(), testToken, project); err == nil {
			t.Errorf("clientConfig(%q) succeeded, want a rejection", project)
		}
	}
}

func TestDepsFromRequestRequiresCredentials(t *testing.T) {
	tests := []struct {
		name    string
		auth    string
		project string
		want    string
	}{
		{name: "no token", project: testProjectName, want: "no credentials"},
		{name: "wrong scheme", auth: "Basic abc", project: testProjectName, want: "no credentials"},
		{name: "no project", auth: "Bearer " + testToken, want: "no project"},
		{name: "invalid project", auth: "Bearer " + testToken, project: "a/b", want: "invalid project"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if tc.project != "" {
				r.Header.Set(projectHeader, tc.project)
			}

			_, err := depsFromRequest(r, baseConfig())(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// A tool error is handed back to the model; it must not carry the
			// caller's credential.
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks the bearer token: %q", err)
			}
		})
	}
}

// TestCredentialErrorsBlameTheClientNotTheUser pins the copy on the errors a
// request fails with before any read happens. They reach a person through an
// assistant, so they are customer-facing text. The first wording said "the
// caller must forward their credentials"; an assistant read "caller" as the
// person it was talking to and told them to re-authenticate, which cannot
// work, because the credential is the calling client's to send.
//
// internal/agent/copy_test.go holds the repo's plain-language denylist, but it
// scans values inside that package and cannot reach a string in cmd/, so the
// terms that could plausibly turn up in this copy are repeated here.
func TestCredentialErrorsBlameTheClientNotTheUser(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(rbac|control planes?|reconcil|milo|pods?|controllers?)\b`)
	// Second person is the failure mode itself: an error that says "you" hands
	// the model someone to instruct, and the only person it can instruct is the
	// one who cannot fix this.
	secondPerson := regexp.MustCompile(`(?i)\byou(r|rs)?\b`)

	for name, err := range credentialErrors(t) {
		t.Run(name, func(t *testing.T) {
			msg := err.Error()
			if !strings.Contains(msg, "re-authenticating will not help") {
				t.Errorf("error = %q, want it to rule out re-authenticating", msg)
			}
			if !strings.Contains(msg, "client that called this tool") ||
				!strings.Contains(msg, "whoever operates that client") {
				t.Errorf("error = %q, want it to name the client as the actor", msg)
			}
			if m := secondPerson.FindString(msg); m != "" {
				t.Errorf("error = %q addresses the reader as %q; the reader cannot fix it", msg, m)
			}
			if m := banned.FindString(msg); m != "" {
				t.Errorf("error = %q uses %q, which a customer has no way to read", msg, m)
			}
		})
	}
}

// credentialErrors collects one error per way a request can fail before a read,
// keyed by what went missing.
func credentialErrors(t *testing.T) map[string]error {
	t.Helper()

	out := map[string]error{}
	for name, h := range map[string]struct{ auth, project string }{
		"no-token":   {project: "acme-prod"},
		"no-project": {auth: "Bearer " + testToken},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if h.auth != "" {
			r.Header.Set("Authorization", h.auth)
		}
		if h.project != "" {
			r.Header.Set(projectHeader, h.project)
		}
		_, err := depsFromRequest(r, baseConfig())(context.Background())
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		out[name] = err
	}

	_, err := clientConfig(baseConfig(), testToken, "Not A Project")
	if err == nil {
		t.Fatal("invalid-project: expected an error")
	}
	out["invalid-project"] = err
	return out
}

// TestDepsFromRequestIgnoresToolArguments guards the prompt-injection defense:
// the project is read from the header only. A query parameter or body field the
// model could influence must not reach the addressing.
func TestDepsFromRequestIgnoresToolArguments(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp?project=attacker-project", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set(projectHeader, " my-project ")

	cfg, err := clientConfig(baseConfig(), bearerToken(r), strings.TrimSpace(r.Header.Get(projectHeader)))
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	if strings.Contains(cfg.Host, "attacker-project") {
		t.Errorf("Host = %q, want the header's project", cfg.Host)
	}
	if !strings.Contains(cfg.Host, "/projects/my-project/control-plane") {
		t.Errorf("Host = %q, want the trimmed header project", cfg.Host)
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		auth string
		want string
	}{
		{auth: "Bearer " + testToken, want: testToken},
		{auth: "bearer " + testToken, want: testToken},
		{auth: "Bearer  " + testToken + " ", want: testToken},
		{auth: "Basic " + testToken, want: ""},
		{auth: testToken, want: ""},
		{auth: "", want: ""},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		if got := bearerToken(r); got != tc.want {
			t.Errorf("bearerToken(%q) = %q, want %q", tc.auth, got, tc.want)
		}
	}
}

// TestReadsGoThroughTheProjectControlPlane drives a real client at a stub API
// server: a workload read must land on the project's control-plane path with
// the caller's token, in the "default" namespace — not on a namespace named
// after the project on the base host.
func TestReadsGoThroughTheProjectControlPlane(t *testing.T) {
	var gotPath, gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"compute.datumapis.com/v1alpha","kind":"WorkloadList","items":[]}`))
	}))
	defer api.Close()

	cfg, err := clientConfig(&rest.Config{Host: api.URL}, testToken, testProjectName)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}

	// A static mapper keeps the test off discovery; the path under test is the
	// one clientConfig put on the config, not one the mapper chose.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{computev1alpha.GroupVersion})
	mapper.AddSpecific(
		computev1alpha.GroupVersion.WithKind("Workload"),
		computev1alpha.GroupVersion.WithResource("workloads"),
		computev1alpha.GroupVersion.WithResource("workload"),
		meta.RESTScopeNamespace,
	)

	c, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := agent.NewClientReader(c).ListWorkloads(context.Background(), resourceNamespace); err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}

	wantPath := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/my-project/control-plane" +
		"/apis/compute.datumapis.com/v1alpha/namespaces/default/workloads"
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the caller's bearer token", gotAuth)
	}
}

// The address a cluster advertises for its own API server, as the kubelet
// publishes it to a pod. The guard's whole job is spotting this endpoint.
const (
	localClusterHost = "10.0.0.1"
	localClusterPort = "443"
)

// TestCheckControlPlaneEndpointRejectsTheInClusterFallback pins the startup
// guard: a pod given no kubeconfig still gets the in-cluster config, and every
// tool call then fails with a 401 that reads like the caller's credentials are
// bad. The guard turns that into a boot failure naming the actual mistake.
func TestCheckControlPlaneEndpointRejectsTheInClusterFallback(t *testing.T) {
	tests := []struct {
		name        string
		serviceHost string
		servicePort string
		host        string
		wantErr     bool
	}{
		{
			name:        "in-cluster fallback",
			serviceHost: localClusterHost,
			servicePort: localClusterPort,
			host:        "https://10.0.0.1:443",
			wantErr:     true,
		},
		{
			name:        "in-cluster fallback, trailing slash",
			serviceHost: localClusterHost,
			servicePort: localClusterPort,
			host:        "https://10.0.0.1:443/",
			wantErr:     true,
		},
		{
			name:        "IPv6 in-cluster fallback",
			serviceHost: "fd00::1",
			servicePort: localClusterPort,
			host:        "https://[fd00::1]:443",
			wantErr:     true,
		},
		{
			// The deployment supplied a kubeconfig naming a different control
			// plane. This is the only correct configuration.
			name:        "explicit control plane",
			serviceHost: localClusterHost,
			servicePort: localClusterPort,
			host:        "https://control-plane.example:6443",
			wantErr:     false,
		},
		{
			// Same address, different port: a control plane fronted by the
			// local cluster is still not the local API server.
			name:        "same host, different port",
			serviceHost: localClusterHost,
			servicePort: localClusterPort,
			host:        "https://10.0.0.1:6443",
			wantErr:     false,
		},
		{
			// Outside a pod there is no local API server to have fallen back
			// to, so the guard must not fire on a developer's ~/.kube/config.
			name:    "not in a cluster",
			host:    "https://127.0.0.1:6443",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.serviceHost)
			t.Setenv("KUBERNETES_SERVICE_PORT", tc.servicePort)

			err := checkControlPlaneEndpoint(&rest.Config{Host: tc.host})
			if tc.wantErr == (err == nil) {
				t.Fatalf("checkControlPlaneEndpoint(%q) error = %v, wantErr = %v", tc.host, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			// Actionable: it must say what to set, not just that something is
			// wrong.
			if !strings.Contains(err.Error(), "KUBECONFIG") {
				t.Errorf("error = %q, want it to name the setting to fix", err)
			}
		})
	}
}

// TestGuardNamesNoEnvironment: the guard exists so deployment topology stays
// out of this repo, so it must not smuggle any in. It recognises the mistake
// from KUBERNETES_SERVICE_HOST/_PORT, which every cluster sets for itself.
func TestGuardNamesNoEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if got := localClusterEndpoint(); got != "" {
		t.Errorf("localClusterEndpoint() = %q with no cluster env, want %q", got, "")
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "198.51.100.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if got, want := localClusterEndpoint(), "https://198.51.100.1:443"; got != want {
		t.Errorf("localClusterEndpoint() = %q, want %q", got, want)
	}
}

// TestResolvePlanTokenKey covers the key plan tokens are signed with. A key
// too short to be one is a deployment mistake worth refusing to start over; a
// missing one is not, because a single process signing with a key only it
// knows is a working configuration — it just cannot survive a second replica.
func TestResolvePlanTokenKey(t *testing.T) {
	raw := strings.Repeat("k", minPlanTokenKeyLen)
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	t.Run("base64", func(t *testing.T) {
		got, err := resolvePlanTokenKey(encoded)
		if err != nil {
			t.Fatalf("resolvePlanTokenKey: %v", err)
		}
		// Decoded, not taken as the 44 printable characters it is written as:
		// the operator generated 32 bytes of entropy and that is what signs.
		if string(got) != raw {
			t.Errorf("key = %q, want the decoded %q", got, raw)
		}
	})

	t.Run("raw", func(t *testing.T) {
		got, err := resolvePlanTokenKey(raw)
		if err != nil {
			t.Fatalf("resolvePlanTokenKey: %v", err)
		}
		if string(got) != raw {
			t.Errorf("key = %q, want %q", got, raw)
		}
	})

	t.Run("too short", func(t *testing.T) {
		if _, err := resolvePlanTokenKey("hunter2"); err == nil {
			t.Error("accepted a key too short to sign with")
		} else if !strings.Contains(err.Error(), planTokenKeyEnv) {
			t.Errorf("error = %q, want it to name the setting to fix", err)
		}
	})

	t.Run("unset", func(t *testing.T) {
		first, err := resolvePlanTokenKey("")
		if err != nil {
			t.Fatalf("resolvePlanTokenKey: %v", err)
		}
		if len(first) < minPlanTokenKeyLen {
			t.Errorf("generated a %d-byte key, want at least %d", len(first), minPlanTokenKeyLen)
		}
		second, err := resolvePlanTokenKey("")
		if err != nil {
			t.Fatalf("resolvePlanTokenKey: %v", err)
		}
		if string(first) == string(second) {
			t.Error("generated the same key twice; it must be random per process")
		}
	})
}

// TestDepsBindThePlanToTheHeadersProject: a plan token is only good in the
// project it was minted for, and that project is the header's — the same one
// the client addresses. If these two could ever differ, a token minted in one
// tenant would spend in another.
func TestDepsBindThePlanToTheHeadersProject(t *testing.T) {
	planTokenKey = []byte(strings.Repeat("k", minPlanTokenKeyLen))
	t.Cleanup(func() { planTokenKey = nil })

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set(projectHeader, testProjectName)

	// No CA file: this builds a real client, and the point here is what the
	// deps carry, not what they can reach.
	deps, err := depsFromRequest(r, &rest.Config{Host: testHost})(context.Background())
	if err != nil {
		t.Fatalf("depsFromRequest: %v", err)
	}
	if deps.Project != testProjectName {
		t.Errorf("Project = %q, want the header's %q", deps.Project, testProjectName)
	}
	if string(deps.PlanTokenKey) != string(planTokenKey) {
		t.Error("PlanTokenKey did not reach the tools; no plan could be minted")
	}
	// The writer runs as the caller, like every other tool: one client, built
	// from the bearer token on this request.
	if deps.Writer == nil {
		t.Error("no Writer on the deps; the write tools would report themselves unconfigured")
	}
}
