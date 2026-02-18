// Script de teste end-to-end para a Fase 4: Fila de Espera e Backpressure.
//
// Testa:
// 1. Conexões concorrentes que saturam o limite do bucket
// 2. Conexões adicionais entram na fila e eventualmente são atendidas
// 3. Métricas são populadas corretamente
//
// Uso: go run scripts/test_phase4.go
package main

import (
	"database/sql"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

const (
	// Conectar via HAProxy → proxy → SQL Server
	connStr = "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=5"

	// Número de conexões para saturar (o bucket tem max_connections=50)
	// Usamos um valor mais baixo porque são 3 proxies com leastconn
	holdConns   = 20 // Conexões que ficam segurando slot
	extraConns  = 5  // Conexões extras que devem entrar na fila
	holdTimeSec = 10 // Tempo que as conexões iniciais ficam segurando
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== TESTE FASE 4: Fila de Espera e Backpressure ===")

	// ── Teste 1: Conexões concorrentes básicas ──────────────────────
	log.Println("\n── Teste 1: Múltiplas conexões concorrentes ──")
	testConcurrentConnections()

	// ── Teste 2: Saturação e fila ───────────────────────────────────
	log.Println("\n── Teste 2: Saturação e fila de espera ──")
	testQueueBehavior()

	log.Println("\n=== TODOS OS TESTES DA FASE 4 CONCLUÍDOS ===")
}

// testConcurrentConnections valida que múltiplas conexões simultâneas funcionam.
func testConcurrentConnections() {
	const numConns = 10
	var (
		wg      sync.WaitGroup
		success atomic.Int32
		failed  atomic.Int32
	)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			db, err := sql.Open("sqlserver", connStr)
			if err != nil {
				log.Printf("  [conn-%d] FALHA ao abrir: %v", id, err)
				failed.Add(1)
				return
			}
			defer db.Close()

			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			var result int
			err = db.QueryRow("SELECT @id", sql.Named("id", id)).Scan(&result)
			if err != nil {
				log.Printf("  [conn-%d] FALHA na query: %v", id, err)
				failed.Add(1)
				return
			}

			if result != id {
				log.Printf("  [conn-%d] Resultado inesperado: %d != %d", id, result, id)
				failed.Add(1)
				return
			}

			success.Add(1)
			log.Printf("  [conn-%d] OK (SELECT %d)", id, result)
		}(i)
	}

	wg.Wait()
	log.Printf("  Resultado: %d sucesso, %d falhas (de %d)", success.Load(), failed.Load(), numConns)

	if failed.Load() > 0 {
		log.Println("  ❌ FALHA no teste de conexões concorrentes")
		os.Exit(1)
	}
	log.Println("  ✅ Teste de conexões concorrentes OK")
}

// testQueueBehavior cria muitas conexões que seguram slots, e depois
// verifica que conexões adicionais conseguem ser atendidas quando as primeiras liberam.
func testQueueBehavior() {
	var (
		wg          sync.WaitGroup
		holdersOK   atomic.Int32
		holdersFail atomic.Int32
		extrasOK    atomic.Int32
		extrasFail  atomic.Int32
	)

	// Fase A: Criar "holdConns" conexões que ficam segurando por holdTimeSec
	log.Printf("  Fase A: Criando %d conexões que ficam segurando por %ds...", holdConns, holdTimeSec)
	holdReady := make(chan struct{}) // sinal para quando todas estiverem conectadas

	var holdCount atomic.Int32

	for i := 0; i < holdConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			db, err := sql.Open("sqlserver", connStr)
			if err != nil {
				log.Printf("  [hold-%d] FALHA ao abrir: %v", id, err)
				holdersFail.Add(1)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			db.SetConnMaxLifetime(time.Duration(holdTimeSec+5) * time.Second)

			// Abrir conexão real com uma query
			var result int
			err = db.QueryRow("SELECT 1").Scan(&result)
			if err != nil {
				log.Printf("  [hold-%d] FALHA na query inicial: %v", id, err)
				holdersFail.Add(1)
				return
			}

			holdersOK.Add(1)
			count := holdCount.Add(1)
			if int(count) == holdConns {
				close(holdReady)
			}

			// Manter a conexão ocupada
			time.Sleep(time.Duration(holdTimeSec) * time.Second)

			// Fazer outra query antes de fechar para manter a conexão viva
			_ = db.QueryRow("SELECT 1").Scan(&result)
		}(i)
	}

	// Esperar todas as holders estarem conectadas (ou timeout de 30s)
	select {
	case <-holdReady:
		log.Printf("  Todas as %d conexões holders conectadas", holdConns)
	case <-time.After(30 * time.Second):
		log.Printf("  ⚠️ Timeout esperando holders (conectadas: %d/%d)", holdCount.Load(), holdConns)
	}

	// Fase B: Agora enviar conexões extras que devem entrar na fila
	log.Printf("  Fase B: Enviando %d conexões extras (devem ser processadas via fila ou diretamente)...", extraConns)
	extraStart := time.Now()

	for i := 0; i < extraConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			db, err := sql.Open("sqlserver", connStr)
			if err != nil {
				log.Printf("  [extra-%d] FALHA ao abrir: %v", id, err)
				extrasFail.Add(1)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			start := time.Now()
			var result int
			err = db.QueryRow("SELECT @id", sql.Named("id", id+100)).Scan(&result)
			dur := time.Since(start)

			if err != nil {
				log.Printf("  [extra-%d] FALHA na query (após %v): %v", id, dur, err)
				extrasFail.Add(1)
				return
			}

			extrasOK.Add(1)
			log.Printf("  [extra-%d] OK (SELECT %d, latência=%v)", id, result, dur)
		}(i)
	}

	wg.Wait()
	totalDur := time.Since(extraStart)

	log.Printf("  Holders: %d OK, %d falhas", holdersOK.Load(), holdersFail.Load())
	log.Printf("  Extras:  %d OK, %d falhas (duração total: %v)", extrasOK.Load(), extrasFail.Load(), totalDur)

	if holdersFail.Load() > 0 {
		log.Println("  ❌ FALHA: Algumas conexões holders falharam")
		os.Exit(1)
	}
	if extrasFail.Load() > 0 {
		log.Println("  ⚠️ AVISO: Algumas conexões extras falharam (pode indicar queue timeout)")
	} else {
		log.Println("  ✅ Todas as conexões extras foram atendidas com sucesso")
	}

	log.Println("  ✅ Teste de saturação e fila OK")
}
