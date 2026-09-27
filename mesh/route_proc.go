//go:build !darwin

package mesh

import "os"

// routeSource reads the kernel route table, as every Linux node always has.
func routeSource() (read func() ([]byte, error), parse func([]byte) (string, error)) {
	return func() ([]byte, error) { return os.ReadFile("/proc/net/route") }, procRouteIface
}
