//go:build !darwin

package mesh

import "context"

func tailnetStatusFallback(context.Context) ([]byte, error) { return nil, errNoStatusFallback }
