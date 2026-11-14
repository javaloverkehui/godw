-- 初始化脚本：建业务库、只读账号。
-- 平台自身的元数据表由 godw migrate 创建，不在这里重复定义。
--
-- 注意：本脚本只在数据卷为空时执行一次。所有语句必须幂等——
-- 一旦这里报错，postgres 容器会直接退出（entrypoint 的 init 阶段失败），
-- 现场表现是「数据库起不来，日志里只有一句 syntax error」。

-- CREATE DATABASE 不能放进 DO 块（必须在事务外执行），用 \gexec 做条件创建。
SELECT 'CREATE DATABASE dw'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'dw')\gexec

-- 只读账号给 BI / 自助查询使用，避免分析查询误写业务库。
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'godw_ro') THEN
    CREATE ROLE godw_ro LOGIN PASSWORD 'godw_ro';
  END IF;
END
$$;

-- CONNECT 是库级权限，可以在当前库授予。
GRANT CONNECT ON DATABASE dw TO godw_ro;

-- 其余授权必须切到 dw 库执行，否则授权落在 postgres 库上，等于没授。
\connect dw

GRANT USAGE ON SCHEMA public TO godw_ro;
-- 只读账号只读：后续由 godw 建的表默认给 SELECT。
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO godw_ro;
