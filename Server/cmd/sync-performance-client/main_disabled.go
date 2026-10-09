//go:build !identitytest

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sync-performance-client requires the synthetic identitytest build")
	os.Exit(2)
}
