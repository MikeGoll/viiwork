package mesh

import (
	"context"
	"os/exec"
	"time"
)

// routeSource asks the routing socket through route(8): macOS has no
// /proc/net/route.
func routeSource() (read func() ([]byte, error), parse func([]byte) (string, error)) {
	read = func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	}
	return read, parseRouteGetDefault
}
