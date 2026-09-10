// SPDX-License-Identifier: AGPL-3.0-only

// Command compute-mcp serves compute's tools over MCP, alongside the knowledge
// and skills an assistant reads before calling them:
//
//	POST /mcp                   Streamable HTTP MCP, stateless
//	GET  /llms-full.txt         Knowledge: the compute resource model
//	GET  /runbooks/<name>.md    Skills: triage procedures
//	GET  /healthz               liveness
//
// One process, because compute's capability document names all of those URLs.
// Only /mcp takes a credential; see docs.go for why the documents do not.
//
// The server holds no credential of its own for the project control plane: it
// reads and writes through a client built from the caller's own bearer token.
// So a tool call can never see or create more than the person who asked, the
// platform's RBAC stays the single enforcement point, and there is no
// impersonation privilege here to escalate with.
//
// The project a request reads is taken from a header, never from a tool
// argument: arguments are chosen by the model, and a model that could name its
// own project would be one prompt-injection away from another tenant's
// workloads. The header is set by the already-authenticated caller.
//
// Two of the published tools can change something — compute_workload_plan and
// compute_workload_apply — and apply only ever creates the manifest a plan
// token was minted for. Those tokens are signed with PLAN_TOKEN_KEY; see
// resolvePlanTokenKey for what a deployment owes it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/agent"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// projectHeader names the project whose control plane a request reads. Set
	// by the caller, not by the model.
	projectHeader = "X-Datum-Project"

	// serverName and serverVersion identify this server to MCP clients.
	serverName    = "datum-compute-mcp"
	serverVersion = "0.1.0"

	// readHeaderTimeout bounds how long a slow client may hold a connection
	// before sending headers.
	readHeaderTimeout = 10 * time.Second

	// misconfiguredClientNote closes every error a request can fail with before
	// a read is attempted. All of them are the calling client's configuration,
	// and the first assistant to relay the old wording told the user to
	// re-authenticate — so the sentence names the actor and rules that out.
	misconfiguredClientNote = "The person who asked did nothing wrong and re-authenticating will not " +
		"help: this is a configuration problem for whoever operates that client"

	// planTokenKeyEnv names the environment variable holding the key plan
	// tokens are signed with. Base64 or raw, at least minPlanTokenKeyLen bytes
	// either way.
	planTokenKeyEnv = "PLAN_TOKEN_KEY"

	// minPlanTokenKeyLen is the shortest key accepted. HMAC-SHA256's block
	// structure gets nothing from a key longer than its 32-byte output, and a
	// shorter one is a weaker signature than the scheme is meant to have.
	minPlanTokenKeyLen = 32

	// resourceNamespace is where compute's objects live inside a project's
	// control plane: the project routes to the control plane, and within it
	// everything is in "default". Mirrors util.ResourceNamespace, not imported
	// because that package pulls in the whole datumctl plugin runtime.
	resourceNamespace = "default"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// planTokenKey signs the plan tokens compute_workload_plan mints and
	// compute_workload_apply checks. Deployment configuration, resolved once at
	// startup: every request reads it, and a key that differs between replicas
	// means a plan minted by one is refused by another.
	planTokenKey []byte
)

// The scheme carries every group a tool reads: compute's own objects for the
// diagnosis walk, plus networks, quota, and — for the locations a project may
// place at — compute's service availability records and the Locations they
// name. A group missing here fails at the first read with a scheme error, which
// says nothing about which tool wanted it.
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))
	utilruntime.Must(locationsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(quotav1alpha1.AddToScheme(scheme))
	utilruntime.Must(servicesv1alpha1.AddToScheme(scheme))
}

func main() {
	var addr string

	flag.StringVar(&addr, "addr", envOr("COMPUTE_MCP_ADDR", ":8080"),
		"address to serve MCP on")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	key, err := resolvePlanTokenKey(os.Getenv(planTokenKeyEnv))
	if err != nil {
		setupLog.Error(err, "refusing to start")
		os.Exit(1)
	}
	planTokenKey = key

	// GetConfig resolves the --kubeconfig flag that controller-runtime
	// registers, then KUBECONFIG, then in-cluster config, then ~/.kube/config.
	// Only the endpoint and CA are used; see clientForToken.
	baseConfig, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to load control plane configuration")
		os.Exit(1)
	}

	if err := checkControlPlaneEndpoint(baseConfig); err != nil {
		setupLog.Error(err, "refusing to start")
		os.Exit(1)
	}

	if err := run(addr, baseConfig); err != nil {
		setupLog.Error(err, "server failed")
		os.Exit(1)
	}
}

// resolvePlanTokenKey returns the key plan tokens are signed with, given the
// environment's value for it.
//
// A missing key is not fatal: the server generates one and runs. A plan token
// is only ever checked by the process that minted it, and one process holding
// a key nobody else knows is exactly what the scheme needs. What it costs is
// that a plan minted by one replica is refused by another, and a restart
// refuses every token outstanding — so the warning says that plainly rather
// than letting an operator discover it as intermittent refusals under a load
// balancer.
func resolvePlanTokenKey(configured string) ([]byte, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		// Base64 first, since a key generated with `openssl rand -base64 32`
		// is 44 printable characters that would otherwise pass the raw check
		// while carrying only 32 bytes of the entropy it was meant to have.
		if decoded, err := base64.StdEncoding.DecodeString(configured); err == nil &&
			len(decoded) >= minPlanTokenKeyLen {
			return decoded, nil
		}
		if len(configured) >= minPlanTokenKeyLen {
			return []byte(configured), nil
		}
		return nil, fmt.Errorf(
			"%s is too short: it must be at least %d bytes, either raw or base64-encoded. Generate one "+
				"with: openssl rand -base64 32", planTokenKeyEnv, minPlanTokenKeyLen)
	}

	key := make([]byte, minPlanTokenKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating a plan token key: %w", err)
	}
	setupLog.Info("no plan token key configured, generated one for this process",
		"warning", "plan tokens will not validate across replicas or survive a restart; set "+
			planTokenKeyEnv+" to the same value on every replica",
		"env", planTokenKeyEnv)
	return key, nil
}

// checkControlPlaneEndpoint refuses to start when the configuration resolved to
// the API server of the cluster this process runs in.
//
// In a pod with no kubeconfig, GetConfig falls back to in-cluster config, and
// clientConfig then hangs a project control-plane path off the local API
// server. Every tool call fails with a 401 that reads like the caller's token
// is bad when the deployment is what is wrong; failing at boot puts the error
// where the mistake is.
//
// The check matches the SHAPE of the mistake, never an address: which control
// plane a deployment reads is deployment configuration, not this repo's.
func checkControlPlaneEndpoint(cfg *rest.Config) error {
	local := localClusterEndpoint()
	if local == "" || !sameEndpoint(cfg.Host, local) {
		return nil
	}
	return fmt.Errorf(
		"control plane endpoint %s is this cluster's own API server: compute-mcp reads Datum "+
			"project control planes, not the cluster it runs in, so the in-cluster fallback is "+
			"never correct. Give the deployment an explicit control plane: mount a kubeconfig "+
			"naming the control plane's address and CA, and point KUBECONFIG at it (or pass "+
			"--kubeconfig)",
		cfg.Host)
}

// localClusterEndpoint returns the API server address in-cluster config
// resolves to, or "" outside a pod. It mirrors rest.InClusterConfig's own
// derivation rather than calling it, so the check still holds when the
// projected ServiceAccount token InClusterConfig also requires is not mounted.
func localClusterEndpoint() string {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

// sameEndpoint compares two API server addresses, ignoring a trailing slash.
func sameEndpoint(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func run(addr string, baseConfig *rest.Config) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			// A server per request, bound to that caller's identity and
			// project. Nothing is shared between callers.
			s := mcp.NewServer(&mcp.Implementation{
				Name:    serverName,
				Version: serverVersion,
			}, nil)
			agent.RegisterTools(s, depsFromRequest(r, baseConfig))
			return s
		},
		// Stateless: no tool needs session state — what a plan settled travels
		// in the token it returns, not in memory here — and it keeps the
		// server robust against client crashes.
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	// Served from this same process, so one deployment satisfies every URL
	// compute's capability document points the assistant at.
	docs, err := newKnowledgeHandler()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle(knowledgePath, docs)
	mux.Handle(runbookPrefix, docs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	setupLog.Info("listening", "addr", addr, "mcp", "/mcp", "docs", docs.paths())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving on %s: %w", addr, err)
	}
	return nil
}

// depsFromRequest returns a DepsFor that resolves the caller's identity and
// project from r. Resolution is deferred to call time so a request carrying no
// credentials fails as a tool error the model can report, rather than as a nil
// client panic deep in a handler.
func depsFromRequest(r *http.Request, baseConfig *rest.Config) agent.DepsFor {
	token := bearerToken(r)
	project := strings.TrimSpace(r.Header.Get(projectHeader))

	return func(context.Context) (agent.ToolDeps, error) {
		if token == "" {
			return agent.ToolDeps{}, fmt.Errorf(
				"no credentials on this request: the client that called this tool did not forward "+
					"the user's identity. %s", misconfiguredClientNote)
		}
		if project == "" {
			return agent.ToolDeps{}, fmt.Errorf(
				"no project on this request: the client that called this tool did not set the %s "+
					"header. %s", projectHeader, misconfiguredClientNote)
		}

		c, err := clientForToken(baseConfig, token, project)
		if err != nil {
			return agent.ToolDeps{}, err
		}
		// One client serves all three: the reads a tool makes are the reads the
		// person who asked could make themselves, and a workload it creates is
		// one they could have created themselves, whichever tool does it. The
		// Discoverer is given no platform client — this server holds no
		// credential of its own, so quota display units fall back rather than
		// being fetched with an identity the caller does not have.
		return agent.ToolDeps{
			Reader:     agent.NewClientReader(c),
			Discoverer: agent.NewClientDiscoverer(c),
			Writer:     agent.NewClientWriter(c),
			Namespace:  resourceNamespace,
			// The project a plan is bound to is the header's, the same one the
			// client above addresses, so a token minted for one project can
			// never be spent in another.
			Project:      project,
			PlanTokenKey: planTokenKey,
		}, nil
	}
}

// projectControlPlanePath returns the API path for a project's control plane. A
// project is addressed by rewriting the host path, not by a namespace named
// after it — the same rewrite internal/quota, internal/referenceddata and
// internal/cmd/compute/util perform.
func projectControlPlanePath(project string) string {
	return fmt.Sprintf("/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane", project)
}

// clientConfig derives the REST config one request reads through: the caller's
// token, pointed at their project control plane. The base config supplies the
// endpoint and CA only — every credential field it might carry is cleared
// first, so the server's own identity can never leak into a caller's read.
func clientConfig(baseConfig *rest.Config, token, project string) (*rest.Config, error) {
	// The project arrives in a header and is interpolated into a URL path, so
	// it is validated before it can reshape that path into another API route.
	if errs := validation.IsDNS1123Subdomain(project); len(errs) > 0 {
		return nil, fmt.Errorf("invalid project %q on the %s header sent by the client that called "+
			"this tool: %s. %s", project, projectHeader, strings.Join(errs, "; "), misconfiguredClientNote)
	}

	cfg := rest.AnonymousClientConfig(rest.CopyConfig(baseConfig))
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""

	host, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing control plane host: %w", err)
	}
	host.Path = projectControlPlanePath(project)
	cfg.Host = host.String()

	return cfg, nil
}

// clientForToken builds a client that reads project's control plane as the
// bearer of token.
func clientForToken(baseConfig *rest.Config, token, project string) (client.Client, error) {
	cfg, err := clientConfig(baseConfig, token, project)
	if err != nil {
		return nil, err
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building client for caller: %w", err)
	}
	return c, nil
}

// bearerToken extracts a bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
