package meshapi

import "net/url"

// Endpoint paths every node serves. Constants, because both ends of the wire
// use them: a node routes on them and, as a peer, dials them.
const (
	PathStatus          = "/v1/status"
	PathCluster         = "/v1/cluster"
	PathCapacity        = "/v1/capacity"
	PathFleetCapacity   = "/v1/fleet/capacity"
	PathModels          = "/v1/models"
	PathChatCompletions = "/v1/chat/completions"
	PathCompletions     = "/v1/completions"
	PathEmbeddings      = "/v1/embeddings"
	PathHealth          = "/health"
	PathActivity        = "/v1/activity"
	PathActivityStream  = "/v1/activity/stream"
	PathMeshStream      = "/v1/mesh/stream"
	PathPrompts         = "/v1/prompts"
	PathMeshPrompt      = "/v1/mesh/prompt"
	PathPower           = "/v1/power"
	PathMeshPower       = "/v1/mesh/power"

	// PathUpdate is GET (status); stage, activate and rollback are POSTs
	// below it, authorised like alias writes.
	PathUpdate         = "/v1/update"
	PathUpdateStage    = "/v1/update/stage"
	PathUpdateActivate = "/v1/update/activate"
	PathUpdateRollback = "/v1/update/rollback"

	// PathAliases is GET (list) on its own, and PUT/DELETE on
	// PathAliases+"/<name>"; revert is POST on AliasRevertPath(name).
	PathAliases       = "/v1/aliases"
	AliasRevertSuffix = "/revert"
)

// Named SSE events on PathMeshStream.
const (
	SSEActivity = "activity" // carries a MeshEvent
	SSECluster  = "cluster"  // carries a ClusterResponse
)

// AliasPath is the write endpoint for one alias, with the name path-escaped.
func AliasPath(name string) string { return PathAliases + "/" + url.PathEscape(name) }

// AliasRevertPath is the revert endpoint for one alias.
func AliasRevertPath(name string) string { return AliasPath(name) + AliasRevertSuffix }
