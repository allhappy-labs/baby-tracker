//go:build !identitytest

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sync-capacity-client requires an identitytest build and owned disposable resources")
	os.Exit(2)
}
