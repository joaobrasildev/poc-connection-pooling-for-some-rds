// Package main is the entrypoint for the load generator.
// This will simulate coreVMs connecting to the proxy via TDS (Phase 6).
package main

import (
	"fmt"
	"log"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	fmt.Println("Load Generator - Not implemented yet (Phase 6)")
	fmt.Println("Usage: loadgen --total-connections 1000 --buckets 5 --query-mix mixed")
}
