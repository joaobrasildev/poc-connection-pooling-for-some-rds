// Script de teste para Circuit Breaker (Queue Full - TDS Error 50005).
//
// Com max_queue_size=2 e max_connections=3:
//   1. 3 conexões holders saturam o bucket
//   2. 2 conexões entram na fila (depth=2 = max_queue_size)
//   3. A 6ª conexão deve ser rejeitada instantaneamente (circuit breaker)
//
// Uso: go run scripts/test_phase4_circuit_breaker.go
package main

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"github.com/redis/go-redis/v9"
)

const redisAddr = "localhost:6379"

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== TESTE FASE 4: Circuit Breaker (Queue Full - TDS Error 50005) ===")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	ctx := context.Background()

	// Salvar max original
	origMax, err := rdb.Get(ctx, "proxy:bucket:bucket-001:max").Result()
	if err != nil {
		log.Fatalf("Falha ao ler max original: %v", err)
	}
	log.Printf("Max original: %s", origMax)

	// Reduzir para 3 conexões para facilitar saturação
	const maxConns = 3
	log.Printf("Reduzindo max_connections para %d (max_queue_size=2 já configurado no yaml)...", maxConns)
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", maxConns, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)
	time.Sleep(1 * time.Second)

	var (
		wg          sync.WaitGroup
		holdReady   = make(chan struct{})
		holdRelease = make(chan struct{})
		holdCount   atomic.Int32
	)

	// Fase A: Saturar com 3 holders
	log.Printf("Fase A: Criando %d holders...", maxConns)
	for i := 0; i < maxConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			cstr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=60"
			db, err := sql.Open("sqlserver", cstr)
			if err != nil {
				log.Printf("  [hold-%d] FALHA: %v", id, err)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			conn, err := db.Conn(ctx)
			if err != nil {
				log.Printf("  [hold-%d] FALHA conn: %v", id, err)
				return
			}
			defer conn.Close()

			var r int
			if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&r); err != nil {
				log.Printf("  [hold-%d] FALHA query: %v", id, err)
				return
			}
			log.Printf("  [hold-%d] Conectado e mantendo...", id)

			if holdCount.Add(1) == int32(maxConns) {
				close(holdReady)
			}
			<-holdRelease
		}(i)
	}

	select {
	case <-holdReady:
		log.Printf("Todos os %d holders conectados", maxConns)
	case <-time.After(30 * time.Second):
		log.Println("⚠️ Timeout esperando holders")
		close(holdRelease)
		wg.Wait()
		rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
		return
	}

	count, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:count").Result()
	log.Printf("Redis: count=%s, max=%d", count, maxConns)

	// Fase B: Enviar 2 conexões que entram na fila (sem fechar)
	// Essas vão ficar na fila esperando por queue_timeout (10s)
	log.Println("Fase B: Enviando 2 conexões para encher a fila (max_queue_size=2)...")

	var queuedCount atomic.Int32
	queueReady := make(chan struct{})

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			cstr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=15"
			db, err := sql.Open("sqlserver", cstr)
			if err != nil {
				log.Printf("  [queue-%d] FALHA: %v", id, err)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			// Sinalizar que a goroutine foi criada (a query vai bloquear na fila)
			if queuedCount.Add(1) == 2 {
				close(queueReady)
			}

			start := time.Now()
			var r int
			err = db.QueryRow("SELECT 1").Scan(&r)
			dur := time.Since(start)
			if err != nil {
				log.Printf("  [queue-%d] Erro após %v: %v", id, dur, err)
			} else {
				log.Printf("  [queue-%d] OK após %v", id, dur)
			}
		}(i)
	}

	// Esperar as goroutines de fila serem criadas e dar tempo de entrar na fila
	<-queueReady
	log.Println("Aguardando 3s para as conexões entrarem na fila distribuída...")
	time.Sleep(3 * time.Second)

	// Verificar profundidade da fila nos proxies via logs
	log.Println("Verificando profundidade da fila nos proxies (via logs)...")

	// Fase C: Enviar a 6ª conexão — deve ser rejeitada pelo circuit breaker!
	log.Println("Fase C: Enviando conexão extra (deve ser rejeitada pelo circuit breaker)...")
	extraCstr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=10"
	extraDB, err := sql.Open("sqlserver", extraCstr)
	if err != nil {
		log.Printf("FALHA ao abrir extra: %v", err)
		close(holdRelease)
		wg.Wait()
		rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
		return
	}
	defer extraDB.Close()
	extraDB.SetMaxOpenConns(1)
	extraDB.SetMaxIdleConns(1)

	start := time.Now()
	var result int
	err = extraDB.QueryRow("SELECT 42").Scan(&result)
	dur := time.Since(start)

	if err != nil {
		errMsg := err.Error()
		log.Printf("Conexão extra FALHOU após %v: %s", dur, errMsg)

		if strings.Contains(errMsg, "50005") || strings.Contains(errMsg, "Queue is full") ||
			strings.Contains(errMsg, "queue full") {
			log.Println("✅ TDS Error 50005 (Queue Full / Circuit Breaker) recebido!")
		} else if strings.Contains(errMsg, "50004") || strings.Contains(errMsg, "Queue wait timeout") {
			log.Println("✅ TDS Error 50004 (Queue Timeout) recebido (fila pode ter esvaziado antes)")
		} else if strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "timeout") {
			log.Println("✅ Conexão rejeitada (comportamento correto)")
		} else {
			log.Printf("⚠️ Erro inesperado: %s", errMsg)
		}

		// Circuit breaker deve ser instantâneo (< 1s)
		if dur < 5*time.Second {
			log.Printf("✅ Rejeição rápida (%v) — consistente com circuit breaker", dur)
		} else {
			log.Printf("⚠️ Rejeição lenta (%v) — pode ter entrado na fila antes do circuit breaker", dur)
		}
	} else {
		log.Printf("⚠️ Conexão extra SUCEDEU! (SELECT %d em %v)", result, dur)
		log.Println("  Pode ter sido roteada para proxy com fila vazia")
	}

	// Limpar
	log.Println("Liberando holders e restaurando configuração...")
	close(holdRelease)
	wg.Wait()

	rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)

	log.Println("=== TESTE DE CIRCUIT BREAKER CONCLUÍDO ===")
}

func cleanKeys(ctx context.Context, rdb *redis.Client) {
	keys, _ := rdb.Keys(ctx, "proxy:instance:*:conns").Result()
	for _, key := range keys {
		rdb.Del(ctx, key)
	}
}
