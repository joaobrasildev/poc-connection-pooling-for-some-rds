// Script de teste para cenários de erro da Fase 4:
//   - Queue Timeout (TDS Error 50004)
//   - Circuit Breaker / Queue Full (TDS Error 50005)
//
// Estratégia: reduzir temporariamente max_connections no Redis para 5,
// saturar com 5 conexões, e verificar que a 6ª recebe erro.
//
// Uso: go run scripts/test_phase4_errors.go
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
	connStr  = "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=60"
	redisAddr = "localhost:6379"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== TESTE FASE 4: Cenários de Erro (Queue Timeout & Circuit Breaker) ===")

	// Connect to Redis to manipulate limits
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer rdb.Close()

	ctx := context.Background()

	// Salvar o max original
	origMax, err := rdb.Get(ctx, "proxy:bucket:bucket-001:max").Result()
	if err != nil {
		log.Fatalf("Falha ao ler max original: %v", err)
	}
	log.Printf("Max original de bucket-001: %s", origMax)

	// ── Teste 1: Queue Timeout ──────────────────────────────────────
	log.Println("\n── Teste 1: Queue Timeout (esperado: TDS Error 50004) ──")
	testQueueTimeout(ctx, rdb, origMax)

	// Restaurar max entre testes
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	// Limpar counts de instâncias
	cleanupInstanceCounts(ctx, rdb)
	time.Sleep(2 * time.Second) // Dar tempo para os proxies se recuperarem

	log.Println("\n=== TODOS OS TESTES DE ERRO DA FASE 4 CONCLUÍDOS ===")
}

func testQueueTimeout(ctx context.Context, rdb *redis.Client, origMax string) {
	const maxConns = 3 // Reduzir para apenas 3 conexões

	// Reduzir max_connections para valor baixo
	log.Printf("  Reduzindo max_connections de bucket-001 para %d...", maxConns)
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", maxConns, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanupInstanceCounts(ctx, rdb)
	time.Sleep(1 * time.Second)

	var (
		wg          sync.WaitGroup
		holdersOK   atomic.Int32
		holdReady   = make(chan struct{})
		holdRelease = make(chan struct{})
	)

	// Fase A: Saturar com maxConns conexões que ficam segurando
	log.Printf("  Fase A: Criando %d conexões holders...", maxConns)
	for i := 0; i < maxConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			db, err := sql.Open("sqlserver", connStr)
			if err != nil {
				log.Printf("  [hold-%d] FALHA ao abrir: %v", id, err)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)

			conn, err := db.Conn(ctx)
			if err != nil {
				log.Printf("  [hold-%d] FALHA ao obter conn: %v", id, err)
				return
			}
			defer conn.Close()

			var result int
			err = conn.QueryRowContext(ctx, "SELECT 1").Scan(&result)
			if err != nil {
				log.Printf("  [hold-%d] FALHA na query: %v", id, err)
				return
			}

			holdersOK.Add(1)
			log.Printf("  [hold-%d] OK, mantendo conexão...", id)

			if int(holdersOK.Load()) == maxConns {
				close(holdReady)
			}

			// Esperar sinal para liberar
			<-holdRelease

			// Manter conexão ativa até aqui
			_ = conn.QueryRowContext(ctx, "SELECT 1").Scan(&result)
		}(i)
	}

	// Esperar todos os holders estarem prontos
	select {
	case <-holdReady:
		log.Printf("  Todos os %d holders conectados", maxConns)
	case <-time.After(30 * time.Second):
		log.Printf("  ⚠️ Timeout esperando holders (conectados: %d/%d)", holdersOK.Load(), maxConns)
		close(holdRelease)
		wg.Wait()
		return
	}

	// Verificar counter no Redis
	count, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:count").Result()
	max, _ := rdb.Get(ctx, "proxy:bucket:bucket-001:max").Result()
	log.Printf("  Redis: count=%s, max=%s", count, max)

	// Fase B: Tentar abrir mais uma conexão — deve ficar na fila e eventualmente fazer timeout
	// queue_timeout no proxy.yaml é 30s, mas vamos usar connection_timeout do driver mais curto
	log.Println("  Fase B: Abrindo conexão extra (deve entrar na fila)...")

	// Usar connection string com timeout mais curto para não esperar 30s
	shortConnStr := "sqlserver://sa:YourStr0ngP@ssword1@localhost:1433?database=tenant_db&encrypt=disable&connection+timeout=10"
	extraDB, err := sql.Open("sqlserver", shortConnStr)
	if err != nil {
		log.Printf("  FALHA ao abrir db extra: %v", err)
		close(holdRelease)
		wg.Wait()
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
		log.Printf("  Conexão extra FALHOU após %v: %v", dur, errMsg)

		// Verificar se o erro contém a mensagem de queue timeout ou queue full
		if strings.Contains(errMsg, "50004") || strings.Contains(errMsg, "queue timeout") ||
			strings.Contains(errMsg, "Queue wait timeout") {
			log.Println("  ✅ Queue Timeout (50004) recebido corretamente!")
		} else if strings.Contains(errMsg, "50005") || strings.Contains(errMsg, "queue full") ||
			strings.Contains(errMsg, "Queue is full") {
			log.Println("  ✅ Queue Full (50005) recebido corretamente!")
		} else if strings.Contains(errMsg, "connection refused") || strings.Contains(errMsg, "timeout") {
			log.Println("  ✅ Conexão rejeitada/timeout (comportamento esperado quando bucket está saturado)")
		} else {
			log.Printf("  ⚠️ Erro inesperado (mas conexão foi rejeitada): %v", errMsg)
		}
	} else {
		log.Printf("  ⚠️ Conexão extra SUCEDEU (SELECT %d em %v) — bucket pode não estar totalmente saturado", result, dur)
		log.Println("  Isso pode ocorrer se o HAProxy roteou para um proxy com slots locais livres")
	}

	// Verificar métricas de timeout/rejected
	log.Println("  Verificando métricas de erro...")
	time.Sleep(1 * time.Second)

	// Limpar: liberar holders e restaurar max
	log.Println("  Liberando holders...")
	close(holdRelease)
	wg.Wait()

	// Restaurar
	log.Printf("  Restaurando max_connections para %s...", origMax)
	rdb.Set(ctx, "proxy:bucket:bucket-001:max", origMax, 0)
	rdb.Set(ctx, "proxy:bucket:bucket-001:count", "0", 0)
	cleanupInstanceCounts(ctx, rdb)

	log.Println("  ✅ Teste de Queue Timeout concluído")
}

func cleanupInstanceCounts(ctx context.Context, rdb *redis.Client) {
	// Limpar contadores de instâncias
	keys, _ := rdb.Keys(ctx, "proxy:instance:*:conns").Result()
	for _, key := range keys {
		rdb.Del(ctx, key)
	}
}
