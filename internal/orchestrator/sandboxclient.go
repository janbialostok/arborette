package orchestrator

import (
	"github.com/arborette/arborette/internal/sandboxclient"
)

// The sandbox wire contract and its HTTP client live in internal/sandboxclient,
// shared with the Sleep-Cycle Worker. These aliases keep the orchestrator's
// existing spellings.

type (
	columnDTO            = sandboxclient.Column
	schemaDTO            = sandboxclient.Schema
	TargetBinding        = sandboxclient.TargetBinding
	IntrospectRequest    = sandboxclient.IntrospectRequest
	IntrospectResponse   = sandboxclient.IntrospectResponse
	DocumentTextRequest  = sandboxclient.DocumentTextRequest
	DocumentTextResponse = sandboxclient.DocumentTextResponse
	ExecuteRequest       = sandboxclient.ExecuteRequest
	ExecuteResponse      = sandboxclient.ExecuteResponse
	SandboxError         = sandboxclient.SandboxError
	SandboxClient        = sandboxclient.Client
)

// NewSandboxClient points a client at the sandbox base URL.
var NewSandboxClient = sandboxclient.NewClient
