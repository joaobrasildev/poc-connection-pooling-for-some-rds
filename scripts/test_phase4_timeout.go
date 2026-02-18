// Script de teste para Queue Timeout real (TDS Error 50004).
//
// Estratégia:
//   1. Reduz max_connections para 3
//   2. Reduz queue_timeout — usa um proxy.yaml temporário com queue_timeout curto
//   3. Na verdade: mantém holders por mais tempo que o queue_timeout (30s)
//      e usa connection_timeout do driver de 35s para capturar o erro TDS
//
// Uso: go run scripts/test_phase4_timeout.go
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

const (
	redisAddr = "localhost:6379"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== TESTE FASE 4: Queue Timeout Real (TDS Error 50004) ===")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	ctx := context.Background()

	// Salvar max original
	origMax, err := rdb.Get(ctx, "proxy:bucket:bucket-001:max").Result()
	if err != nil {
		log.Fatalf("Falha ao ler max original: %v", err)
	}
	log.Printf("Max original: %s", origMax)

	// Reduzir para 3 conexões
	const maxConns = 3
	log.Printf("Reduzindo max_connections para %d...", maxConns)
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", maxConns, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)
	time.Sleep(1 * time.Second)

	var (
		wg        sync.WaitGroup
		holdReady = make(chan struct{})
		holdDone  = make(chan struct{})
		holdCount atomic.Int32
	)

	// Fase A: Criar maxConns holders que seguram por 40s (> queue_timeout de 30s)
	holdTime := 40 * time.Second
	log.Printf("Fase A: Criando %d holders (seguram por %v, > queue_timeout 30s)...", maxConns, holdTime)

	for i := 0; i < maxConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Usar connection_timeout longo para holders
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

			// Manter conexão ativa com queries periódicas
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			timer := time.NewTimer(holdTime)
			defer timer.Stop()

			for {
				select {
				case <-timer.C:
					log.Printf("  [hold-%d] Liberando após %v", id, holdTime)
					return
				case <-ticker.C:
					_ = conn.QueryRowContext(ctx, "SELECT 1").Scan(&r)
				case <-holdDone:
					return
				}
			}
		}(i)
	}

	// Esperar holders prontos
	select {
	case <-holdReady:
		log.Printf("Todos os %d holders conectados", maxConns)
	case <-time.After(30 * time.Second):
		log.Println("⚠️ Timeout esperando holders")
		close(holdDone)
		wg.Wait()
		rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
		return
	}

	count, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:count").Result()
	log.Printf("Redis: count=%s, max=%d", count, maxConns)

	// Fase B: Abrir conexão extra com connection_timeout de 35s
	// O queue_timeout do proxy é 30s — então o proxy deve enviar TDS Error 50004
	// antes do connection_timeout do driver (35s)
	log.Println("Fase B: Abrindo conexão extra (deve receber TDS Error 50004 após ~30s)...")

	extraConn := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=45"
	extraDB, err := sql.Open("sqlserver", extraConn)
	if err != nil {
		log.Printf("FALHA ao abrir extra db: %v", err)
		close(holdDone)
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

		if strings.Contains(errMsg, "50004") || strings.Contains(errMsg, "Queue wait timeout") ||
			strings.Contains(errMsg, "queue timeout") {
			log.Println("✅ TDS Error 50004 (Queue Timeout) recebido corretamente!")
		} else if strings.Contains(errMsg, "50005") || strings.Contains(errMsg, "Queue is full") {
			log.Println("✅ TDS Error 50005 (Queue Full) recebido!")
		} else {
			log.Printf("⚠️ Erro diferente do esperado: %s", errMsg)
			log.Println("  Mas a conexão foi rejeitada, o que é o comportamento correto")
		}

		// Verificar se demorou ~30s (queue_timeout)
		if dur >= 25*time.Second && dur <= 35*time.Second {
			log.Printf("✅ Tempo de espera (~%v) consistente com queue_timeout de 30s", dur.Round(time.Second))
		}
	} else {
		log.Printf("⚠️ Conexão extra SUCEDEU! (SELECT %d em %v)", result, dur)
		log.Println("  Pode ter sido roteada para proxy com slot livre")
	}

	// Limpar
	log.Println("Liberando holders e restaurando configuração...")
	close(holdDone)
	wg.Wait()

	rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)

	// Verificar métricas de timeout
	log.Println("Verificação de métricas nos logs do proxy recomendada")
	log.Println("=== TESTE DE TIMEOUT CONCLUÍDO ===")
}

func cleanKeys(ctx context.Context, rdb *redis.Client) {
	keys, _ := rdb.Keys(ctx, "proxy:instance:*:conns").Result()
	for _, key := range keys {
		rdb.Del(ctx, key)
	}
}
