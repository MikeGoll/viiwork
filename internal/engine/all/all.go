// Package all registers every engine viiwork ships. Importing it for its side
// effect is how a binary, or a test that parses configs, learns the engines:
//
//	import _ "github.com/janit/viiwork/v2/internal/engine/all"
//
// This file is the one place an engine is listed (contract C7). Adding an
// engine is its own package plus one line here; the node, viiwork-accept and
// the acceptance tests all import this package, so none of them changes.
package all

import (
	_ "github.com/janit/viiwork/v2/internal/engine/freetoken" // FreeToken
	_ "github.com/janit/viiwork/v2/internal/engine/llamacpp"  // llama.cpp, the reference engine
	_ "github.com/janit/viiwork/v2/internal/engine/vllm"      // vLLM
)
