-- init-databases.sql
-- Initializes tenant databases and test objects on each SQL Server container.

IF NOT EXISTS (
    SELECT name
    FROM sys.databases
    WHERE
        name = 'tenant_db'
)
BEGIN
CREATE DATABASE tenant_db;

PRINT 'Database tenant_db created.';

END

USE tenant_db;
GO

IF NOT EXISTS (
    SELECT *
    FROM sys.objects
    WHERE
        object_id = OBJECT_ID (N '[dbo].[tenants]')
        AND type = 'U'
)
BEGIN
CREATE TABLE dbo.tenants (
    id INT IDENTITY (1, 1) PRIMARY KEY,
    app_id NVARCHAR (100) NOT NULL UNIQUE,
    name NVARCHAR (255) NOT NULL,
    bucket_id NVARCHAR (50) NOT NULL,
    created_at DATETIME2 DEFAULT GETUTCDATE (),
    updated_at DATETIME2 DEFAULT GETUTCDATE (),
    is_active BIT DEFAULT 1
);

CREATE INDEX IX_tenants_bucket_id ON dbo.tenants (bucket_id);

CREATE INDEX IX_tenants_app_id ON dbo.tenants (app_id);

PRINT 'Table tenants created.';

END

IF NOT EXISTS (SELECT * FROM sys.objects WHERE object_id = OBJECT_ID(N'[dbo].[orders]') AND type = 'U')
BEGIN
    CREATE TABLE dbo.orders (
        id INT IDENTITY(1,1) PRIMARY KEY,
        tenant_id INT NOT NULL REFERENCES dbo.tenants(id),
        order_number NVARCHAR(50) NOT NULL,
        total_amount DECIMAL(18,2) NOT NULL DEFAULT 0,
        status NVARCHAR(20) NOT NULL DEFAULT 'pending',
        created_at DATETIME2 DEFAULT GETUTCDATE(),
        updated_at DATETIME2 DEFAULT GETUTCDATE()
    );
    CREATE INDEX IX_orders_tenant_id ON dbo.orders(tenant_id);
    PRINT 'Table orders created.';
END
GO

IF NOT EXISTS (
    SELECT *
    FROM sys.objects
    WHERE
        object_id = OBJECT_ID (N '[dbo].[order_items]')
        AND type = 'U'
)
BEGIN
CREATE TABLE dbo.order_items (
    id INT IDENTITY (1, 1) PRIMARY KEY,
    order_id INT NOT NULL REFERENCES dbo.orders (id),
    product_name NVARCHAR (255) NOT NULL,
    quantity INT NOT NULL DEFAULT 1,
    unit_price DECIMAL(18, 2) NOT NULL DEFAULT 0,
    created_at DATETIME2 DEFAULT GETUTCDATE ()
);

CREATE INDEX IX_order_items_order_id ON dbo.order_items (order_id);

PRINT 'Table order_items created.';

END

IF OBJECT_ID('dbo.sp_get_tenant', 'P') IS NOT NULL
    DROP PROCEDURE dbo.sp_get_tenant;
GO

CREATE PROCEDURE dbo.sp_get_tenant @app_id NVARCHAR(100)
AS
BEGIN
    SET NOCOUNT ON;
    SELECT id, app_id, name, bucket_id, created_at, updated_at, is_active
    FROM dbo.tenants WHERE app_id = @app_id;
END
GO

IF OBJECT_ID ('dbo.sp_create_order', 'P') IS NOT NULL
DROP PROCEDURE dbo.sp_create_order;
GO

CREATE PROCEDURE dbo.sp_create_order @tenant_id INT, @order_number NVARCHAR(50), @total_amount DECIMAL(18,2)
AS
BEGIN
    SET NOCOUNT ON;
    BEGIN TRY
        BEGIN TRANSACTION;
        INSERT INTO dbo.orders (tenant_id, order_number, total_amount, status)
        VALUES (@tenant_id, @order_number, @total_amount, 'pending');
        DECLARE @order_id INT = SCOPE_IDENTITY();
        SELECT @order_id AS order_id;
        COMMIT TRANSACTION;
    END TRY
    BEGIN CATCH
        IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION;
        THROW;
    END CATCH
END
GO

IF OBJECT_ID ('dbo.sp_connection_info', 'P') IS NOT NULL
DROP PROCEDURE dbo.sp_connection_info;
GO

CREATE PROCEDURE dbo.sp_connection_info
AS
BEGIN
    SET NOCOUNT ON;
    SELECT @@SPID AS session_id, SUSER_SNAME() AS login_name, DB_NAME() AS database_name,
           HOST_NAME() AS host_name, APP_NAME() AS app_name, GETUTCDATE() AS server_time,
           @@VERSION AS server_version;
END
GO

PRINT 'All stored procedures created.';

PRINT 'Database initialization complete.';
GO