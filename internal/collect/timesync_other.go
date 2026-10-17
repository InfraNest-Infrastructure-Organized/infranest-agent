//go:build !linux

package collect

func readTimeSync() (*bool, *int64) { return nil, nil }
