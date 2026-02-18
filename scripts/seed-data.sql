-- seed-data.sql
-- Seeds test data into the tenant_db database.
-- Run after init-databases.sql.

USE tenant_db;
GO

-- Insert sample tenants (only if not already seeded)
IF NOT EXISTS (SELECT 1 FROM dbo.tenants WHERE app_id = 'APP-000001')
BEGIN
    DECLARE @i INT = 1;
    WHILE @i <= 100
    BEGIN
        INSERT INTO dbo.tenants (app_id, name, bucket_id)
        VALUES (
            CONCAT('APP-', RIGHT('000000' + CAST(@i AS NVARCHAR(6)), 6)),
            CONCAT('Test Application ', @i),
            CONCAT('bucket-', RIGHT('000' + CAST(((@i - 1) % 3 + 1) AS NVARCHAR(3)), 3))
        );
        SET @i = @i + 1;
    END
    PRINT 'Seeded 100 test tenants.';
END
ELSE
    PRINT 'Tenants already seeded, skipping.';
GO

-- Insert sample orders
IF NOT EXISTS (SELECT 1 FROM dbo.orders WHERE order_number = 'ORD-000001')
BEGIN
    DECLARE @j INT = 1;
    WHILE @j <= 50
    BEGIN
        INSERT INTO dbo.orders (tenant_id, order_number, total_amount, status)
        VALUES (
            ((@j - 1) % 100) + 1,
            CONCAT('ORD-', RIGHT('000000' + CAST(@j AS NVARCHAR(6)), 6)),
            CAST(RAND(CHECKSUM(NEWID())) * 1000 AS DECIMAL(18, 2)),
            CASE (@j % 3)
                WHEN 0 THEN 'completed'
                WHEN 1 THEN 'pending'
                ELSE 'processing'
            END
        );
        SET @j = @j + 1;
    END
    PRINT 'Seeded 50 test orders.';
END
ELSE
    PRINT 'Orders already seeded, skipping.';
GO

-- Insert sample order items
IF NOT EXISTS (SELECT 1 FROM dbo.order_items WHERE order_id = 1)
BEGIN
    DECLARE @k INT = 1;
    WHILE @k <= 50
    BEGIN
        INSERT INTO dbo.order_items (order_id, product_name, quantity, unit_price)
        VALUES (
            @k,
            CONCAT('Product-', CAST(CEILING(RAND(CHECKSUM(NEWID())) * 20) AS NVARCHAR(5))),
            CEILING(RAND(CHECKSUM(NEWID())) * 10),
            CAST(RAND(CHECKSUM(NEWID())) * 100 AS DECIMAL(18, 2))
        );
        SET @k = @k + 1;
    END
    PRINT 'Seeded 50 test order items.';
END
ELSE
    PRINT 'Order items already seeded, skipping.';
GO

PRINT 'Seed data complete.';
GO
