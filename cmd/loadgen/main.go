// Package main é o ponto de entrada para o gerador de carga.
// Irá simular coreVMs conectando ao proxy via TDS (Fase 6).
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
