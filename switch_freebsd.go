//go:build freebsd

package main

import "fmt"

func switchSystem(nixBin, system string) error {
	return fmt.Errorf("FreeBSD support not yet implemented")
}

func mountSource(path string) (string, string) {
	return "", ""
}
