package freetoken

import (
	"fmt"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
)

// Options is the `freetoken:` block.
//
// Deliberately short, and the source repo says why: FreeToken resolves dtype,
// attention backend, MoE cache size, KV capacity, page size and CUDA-graph
// sizes from the checkpoint and the card, and does it better than a config file
// can. What is generated is the set the NODE decides — where to listen, what to
// call the model on the mesh, and the knobs whose value is published to the
// mesh. Everything else belongs in models[].args.
type Options struct {
	Binary      string  `yaml:"binary"`
	MemoryRatio float64 `yaml:"memory_ratio"`
	// MoEBackend generates `--moe-strategy`, which is what 0.1.3 calls it. The
	// key keeps the older name: C1 freezes the YAML an operator writes, and a
	// second key spelled moe_strategy would buy a rule about which wins.
	MoEBackend string `yaml:"moe_backend"`
	// KVReserveTokens is the KV token floor the engine holds back before
	// `--moe-cache-auto` fills MoE experts.
	//
	// Zero is not "no reserve" — it means the flag is not generated and the
	// engine's own default of 8192 tokens stands. That default is small by
	// design, because the expert cache has priority over KV.
	//
	// This key is NOT inert, though this package said it was until 0.1.3 was
	// read properly. viiwork never generates --moe-cache-auto, but the engine
	// turns it on itself for every offload-family strategy (offload, cpu,
	// hybrid) that was given no cache-sizing flag — which is what
	// moe_backend: auto resolves to on a MoE model. So on the models this
	// engine exists to run, the floor is always live, and leaving it at zero
	// caps KV at 8192 tokens however much context the node publishes.
	//
	// Set it equal to context x parallel on an offload-family model.
	//
	// This also closes spike finding F7, which recorded the flag as
	// unverifiable because it is emitted nowhere in viiwork-freetoken. It is
	// real: FreeToken 0.1.3 documents it, and defaults it to 8192 in
	// freetoken/engine/config.py.
	KVReserveTokens int `yaml:"kv_reserve_tokens"`
}

func defaultOptions() Options {
	return Options{Binary: "ft", MemoryRatio: 0.90, MoEBackend: "auto"}
}

// ValidateOptions is engine.OptionsValidator.
func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return fmt.Errorf("%s.%s: %w", path, name, err)
	}
	if opts.Binary == "" {
		return fmt.Errorf("%s.%s.binary must not be empty", path, name)
	}
	if opts.MemoryRatio <= 0 || opts.MemoryRatio > 1 {
		return fmt.Errorf("%s.%s.memory_ratio %v must be in (0, 1]", path, name, opts.MemoryRatio)
	}
	if strings.TrimSpace(opts.MoEBackend) == "" {
		return fmt.Errorf("%s.%s.moe_backend must not be empty", path, name)
	}
	if opts.KVReserveTokens < 0 {
		return fmt.Errorf("%s.%s.kv_reserve_tokens must be >= 0", path, name)
	}

	// Decision 7: the unit of deployment is the card. The engine has a
	// --tensor-parallel-size flag but rejects more than one --gpu entry.
	//
	// The rule is expressed as len(GPUs) because `gpus_per_backend` is not a
	// Spec field — C7's Spec carries GPUs []int and nothing else about the
	// split. That works because config.ModelSpec passes BackendGPUs(0): at
	// validation time Spec.GPUs is ONE BACKEND's cards, not the model's whole
	// gpus: list. The distinction is the whole rule. Were it the whole list,
	// this would reject the ordinary `gpus: [0,1,2]` + `gpus_per_backend: 1` —
	// three single-card backends, the exact topology this engine wants.
	//
	// That was spike finding F6, left open because it could not be settled
	// from the documents. It is settled now, and pinned by
	// internal/accept.TestFreeTokenAcceptsOneCardPerBackendAcrossManyCards
	// rather than by this comment.
	if len(s.GPUs) > 1 {
		return fmt.Errorf("%s.gpus_per_backend must be 1 for engine %s: the engine rejects more than one card per process (got a backend with %d)", path, name, len(s.GPUs))
	}
	return nil
}
