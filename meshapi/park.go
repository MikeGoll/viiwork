package meshapi

// ParkRequest is the body of PathModelsDown and PathModelsUp. No models
// means every model the node is configured with.
type ParkRequest struct {
	Models []string `json:"models,omitempty"`
}

// ParkResponse answers PathModelsDown and PathModelsUp: each named model's
// state after the change.
type ParkResponse struct {
	Node   string      `json:"node"`
	Models []ModelPark `json:"models"`
}

// ModelPark is one model after a down or up. Changed is false when it was
// already in that state.
type ModelPark struct {
	Name    string `json:"name"`
	Parked  bool   `json:"parked"`
	Changed bool   `json:"changed"`
}
