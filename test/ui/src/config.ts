/**
 * 测试环境配置。
 *
 * 约定：所有可变环境项都从环境变量读，默认值对齐 `config.win.yaml`，
 * 这样在 CI 上换库/换端口不用改脚本。
 */
export const APP = {
  baseURL: process.env.GODW_BASE_URL || 'http://127.0.0.1:8080',
  email: process.env.GODW_EMAIL || 'admin@godw.local',
  password: process.env.GODW_PASSWORD || 'admin12345',
};

export const DB = {
  host: process.env.GODW_DB_HOST || '127.0.0.1',
  port: Number(process.env.GODW_DB_PORT || 5432),
  user: process.env.GODW_DB_USER || 'godw',
  password: process.env.GODW_DB_PASSWORD || 'godw',
  database: process.env.GODW_DB_NAME || 'godw',
};

/** 每轮运行一个独立前缀：并发跑两次也不会互相踩数据。 */
export const RUN_ID = process.env.GODW_E2E_RUN || Date.now().toString(36);

/** 测试自建对象统一前缀，便于清理与人工辨认。 */
export const PREFIX = `e2e_${RUN_ID}`;
