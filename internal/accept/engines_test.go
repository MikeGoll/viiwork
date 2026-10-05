package accept

// Acceptance validates a config file for the node that will run it, so its
// tests must register the same engines a node does. The binary does this in
// cmd/viiwork-accept; this is its test-side counterpart. Both import the one
// list in internal/engine/all, so neither can fall behind the node.
import _ "github.com/janit/viiwork/v2/internal/engine/all"
