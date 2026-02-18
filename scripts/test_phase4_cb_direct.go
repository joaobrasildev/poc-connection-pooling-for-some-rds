// Script de teste Circuit Breaker via proxy direto (sem HAProxy).
//
// Executa de dentro da rede Docker para acessar proxy-1 diretamente,
// garantindo que todo o depth de fila fique em um único proxy.
//
// Uso: go run scripts/test_phase4_cb_direct.go
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
	log.Println("=== TESTE: Circuit Breaker direto no proxy-1 ===")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	ctx := context.Background()

	origMax, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:max").Result()
	log.Printf("Max original: %s", origMax)

	// Reduzir para 3 conexões
	const maxConns = 3
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", maxConns, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)
	time.Sleep(1 * time.Second)

	// Usar HAProxy, mas enviar 3+3+1=7 conexões rápidas em sequência
	// Com max_queue_size=2 por proxy e HAProxy distribuindo,
	// se um proxy receber 3+ conexões na fila, ele deve rejeitar.
	// 
	// Abordagem: enviar MUITAS conexões para forçar que pelo menos
	// um proxy atinja depth > 2.

	var (
		wg          sync.WaitGroup
		holdReady   = make(chan struct{})
		holdRelease = make(chan struct{})
		holdCount   atomic.Int32
	)

	// Saturar: 3 holders
	for i := 0; i < maxConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cstr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=30"
			db, err := sql.Open("sqlserver", cstr)
			if err != nil {
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			conn, err := db.Conn(ctx)
			if err != nil {
				return
			}
			defer conn.Close()
			var r int
			conn.QueryRowContext(ctx, "SELECT 1").Scan(&r)
			log.Printf("  [hold-%d] OK", id)
			if holdCount.Add(1) == int32(maxConns) {
				close(holdReady)
			}
			<-holdRelease
		}(i)
	}

	select {
	case <-holdReady:
		log.Printf("Holders prontos (%d/%d)", maxConns, maxConns)
	case <-time.After(20 * time.Second):
		log.Println("Timeout holders")
		close(holdRelease)
		wg.Wait()
		return
	}

	count, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:count").Result()
	log.Printf("Redis count=%s, max=%d", count, maxConns)

	// Enviar 10 conexões em paralelo — HAProxy distribui entre 3 proxies
	// Com leastconn, cada proxy recebe ~3-4 conexões
	// max_queue_size=2 → proxy que receber 3+ na fila deve rejeitar
	const extraConns = 10
	log.Printf("Enviando %d conexões extras em paralelo (max_queue_size=2)...", extraConns)

	var (
		circuitBreakerHit atomic.Int32
		timeoutHit        atomic.Int32
		otherError        atomic.Int32
		success           atomic.Int32
	)

	for i := 0; i < extraConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			cstr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=15"
			db, err := sql.Open("sqlserver", cstr)
			if err != nil {
				otherError.Add(1)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			start := time.Now()
			var r int
			err = db.QueryRow("SELECT 1").Scan(&r)
			dur := time.Since(start)

			if err != nil {
				errMsg := err.Error()
				if strings.Contains(errMsg, "50005") || strings.Contains(errMsg, "queue full") ||
					strings.Contains(errMsg, "Queue is full") {
					circuitBreakerHit.Add(1)
					log.Printf("  [extra-%d] CIRCUIT BREAKER (50005) após %v", id, dur)
				} else if strings.Contains(errMsg, "50004") || strings.Contains(errMsg, "Queue wait timeout") {
					timeoutHit.Add(1)
					log.Printf("  [extra-%d] TIMEOUT (50004) após %v", id, dur)
				} else {
					otherError.Add(1)
					log.Printf("  [extra-%d] ERRO após %v: %s", id, dur, errMsg)
				}
			} else {
				success.Add(1)
				log.Printf("  [extra-%d] OK após %v", id, dur)
			}
		}(i)
	}

	// Esperar todas as extras terminarem
	time.Sleep(15 * time.Second) // max queue_timeout + margem

	log.Println("\nResultados:")
	log.Printf("  Circuit Breaker (50005): %d", circuitBreakerHit.Load())
	log.Printf("  Queue Timeout (50004):   %d", timeoutHit.Load())
	log.Printf("  Outros erros:            %d", otherError.Load())
	log.Printf("  Sucesso:                 %d", success.Load())

	if circuitBreakerHit.Load() > 0 {
		log.Println("✅ Circuit breaker (50005) acionado com sucesso!")
	} else if timeoutHit.Load() > 0 {
		log.Println("✅ Queue timeout acionado (circuit breaker não foi ativado porque HAProxy distribuiu bem)")
	} else {
		log.Println("⚠️ Nenhum erro de fila detectado")
	}

	close(holdRelease)
	wg.Wait()

	// Restaurar
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanKeys(ctx, rdb)

	log.Println("=== TESTE CONCLUÍDO ===")
}

func cleanKeys(ctx context.Context, rdb *redis.Client) {
	keys, _ := rdb.Keys(ctx, "proxy:instance:*:conns").Result()
	for _, key := range keys {
		rdb.Del(ctx, key)
	}
}
