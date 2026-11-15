import { Pool, QueryResultRow } from 'pg';
import { DB } from './config';

/**
 * 元数据库直连。
 *
 * UI 断言只能证明「页面显示成这样」，不能证明「数据真的写对了」。
 * 每个写操作都配一条 DB 断言，两边不一致就是真的 bug：
 * 要么写漏了，要么读错了。
 */
const pool = new Pool({ ...DB, max: 4 });

export async function sql<T extends QueryResultRow = QueryResultRow>(
  text: string,
  params: unknown[] = [],
): Promise<T[]> {
  const res = await pool.query<T>(text, params as never[]);
  return res.rows;
}

export async function one<T extends QueryResultRow = QueryResultRow>(
  text: string,
  params: unknown[] = [],
): Promise<T | undefined> {
  const rows = await sql<T>(text, params);
  return rows[0];
}

/** 取单值，避免到处写 rows[0].c。 */
export async function scalar<T = number>(text: string, params: unknown[] = []): Promise<T> {
  const row = await one<Record<string, T>>(text, params);
  if (!row) throw new Error('查询无结果: ' + text);
  return Object.values(row)[0] as T;
}

export async function count(table: string, where = 'true', params: unknown[] = []): Promise<number> {
  return Number(await scalar<number>(`SELECT count(*)::int FROM ${table} WHERE ${where}`, params));
}

/**
 * 轮询等待某个 DB 事实成立。
 *
 * 写接口是同步落库的，但页面刷新、调度器认领实例这些动作有先后，
 * 硬 sleep 既慢又脆；用轮询把「最终一致」显式写出来。
 */
export async function waitForDb<T>(
  probe: () => Promise<T>,
  ok: (v: T) => boolean,
  what: string,
  timeoutMs = 10_000,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: T | undefined;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      last = await probe();
      if (ok(last)) return last;
    } catch (err) {
      lastErr = err;
    }
    await new Promise(r => setTimeout(r, 200));
  }
  const detail = lastErr ? `最后错误: ${(lastErr as Error).message}` : `最后取值: ${JSON.stringify(last)}`;
  throw new Error(`等待「${what}」超时（${timeoutMs}ms）；${detail}`);
}

/** 当前登录账号所属工作区：所有业务表都按 workspace_id 隔离，断言必须带上它。 */
let cachedWs: string | null = null;
export async function workspaceId(): Promise<string> {
  if (cachedWs) return cachedWs;
  const { APP } = await import('./config');
  cachedWs = await scalar<string>(
    'SELECT workspace_id FROM app_user WHERE email = $1 ORDER BY created_at LIMIT 1',
    [APP.email],
  );
  return cachedWs;
}
