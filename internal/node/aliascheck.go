package node

import (
	"github.com/janit/viiwork/v2/internal/alias"
	"github.com/janit/viiwork/v2/internal/proxy"
)

// The alias resolver is wired into the proxy here, so the compile-time checks
// that its methods fit the proxy's hooks live here too: internal/alias need
// not know the proxy's hook types.
var (
	_ proxy.Resolver    = (*alias.Resolver)(nil).Resolve
	_ proxy.ModelLister = (*alias.Resolver)(nil).ModelEntries
)
