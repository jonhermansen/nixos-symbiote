//go:build darwin

package main

import "fmt"

func switchSystem(nixBin, system string) error {
	return fmt.Errorf("macOS support not yet implemented")
}

func mountSource(path string) (string, string) {
	return "", ""
}
