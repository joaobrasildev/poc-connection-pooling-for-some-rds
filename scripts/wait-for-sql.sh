#!/usr/bin/env bash
# ============================================================================
# wait-for-sql.sh
# Waits for SQL Server containers to be ready, then runs init + seed scripts.
# Used by the docker-compose init-db service.
# ============================================================================

set -e

SQLCMD="/opt/mssql-tools18/bin/sqlcmd"
if [ ! -f "$SQLCMD" ]; then
    SQLCMD="/opt/mssql-tools/bin/sqlcmd"
fi

# Fallback to sqlcmd in PATH
if [ ! -f "$SQLCMD" ]; then
    SQLCMD="sqlcmd"
fi

SQL_SERVERS=(
    "sqlserver-bucket-1:1433:YourStr0ngP@ssword1"
    "sqlserver-bucket-2:1433:YourStr0ngP@ssword2"
    "sqlserver-bucket-3:1433:YourStr0ngP@ssword3"
)

MAX_RETRIES=30
RETRY_INTERVAL=5

wait_for_sql() {
    local host=$1
    local port=$2
    local password=$3
    local retries=0

    echo "⏳ Waiting for SQL Server at ${host}:${port}..."
    while [ $retries -lt $MAX_RETRIES ]; do
        if $SQLCMD -S "${host},${port}" -U sa -P "${password}" -Q "SELECT 1" -C -b > /dev/null 2>&1; then
            echo "✅ SQL Server at ${host}:${port} is ready!"
            return 0
        fi
        retries=$((retries + 1))
        echo "   Attempt ${retries}/${MAX_RETRIES} - not ready yet, waiting ${RETRY_INTERVAL}s..."
        sleep $RETRY_INTERVAL
    done

    echo "❌ SQL Server at ${host}:${port} failed to start after ${MAX_RETRIES} attempts"
    return 1
}

run_sql_script() {
    local host=$1
    local port=$2
    local password=$3
    local script=$4

    echo "📝 Running ${script} on ${host}:${port}..."
    $SQLCMD -S "${host},${port}" -U sa -P "${password}" -i "${script}" -C -b
    echo "✅ Script ${script} completed on ${host}:${port}"
}

echo "============================================"
echo "  SQL Server Initialization Script"
echo "============================================"

# Wait for all SQL Servers
for server in "${SQL_SERVERS[@]}"; do
    IFS=':' read -r host port password <<< "$server"
    wait_for_sql "$host" "$port" "$password"
done

echo ""
echo "All SQL Servers are ready. Running initialization scripts..."
echo ""

# Run init and seed scripts on each server
for server in "${SQL_SERVERS[@]}"; do
    IFS=':' read -r host port password <<< "$server"

    run_sql_script "$host" "$port" "$password" "/scripts/init-databases.sql"
    run_sql_script "$host" "$port" "$password" "/scripts/seed-data.sql"

    echo "---"
done

echo ""
echo "============================================"
echo "  ✅ All databases initialized and seeded!"
echo "============================================"
