import { defineStore } from 'pinia';
import type { AuthApi } from '../api/auth';
import { isApiError } from '../api/errors';
import type { Me, TenantSummary, UserSummary } from '../api/types';
import { accessFromMe, type Access } from '../registry/visibility';

// 会话状态（只放 admin）。Cookie 会话 `le_admin_sid` 是 HttpOnly，脚本读不到也不需要读；这里只保存「当前身份快照」（/auth/me）与内存里的 CSRF 令牌。
// 不把令牌/口令/TOTP 码写入任何浏览器存储。

export type SessionStatus = 'unknown' | 'authed' | 'anon';

export interface MfaChallenge {
  token: string;
  /** 本地时间戳（毫秒）：5 分钟后过期，界面据此提示重新登录。 */
  expiresAt: number;
}
export const MFA_TTL_MS = 5 * 60 * 1000;

export type LoginOutcome = 'ok' | 'totp_required' | 'must_change';

let authApi: AuthApi | null = null;
/** 由应用装配时注入（避免 store 与 client 循环依赖）；测试注入假 API。 */
export function bindAuthApi(api: AuthApi): void {
  authApi = api;
}
function api(): AuthApi {
  if (!authApi) throw new Error('session: auth API is not bound');
  return authApi;
}

export const useSessionStore = defineStore('session', {
  state: () => ({
    status: 'unknown' as SessionStatus,
    me: null as Me | null,
    csrf: null as string | null,
    mfa: null as MfaChallenge | null,
    /** 代管的目标租户（M1 = 默认租户，D-13）；未能解析（无 tenant:read）时为 null，横幅退回通用文案。 */
    actingTenant: null as TenantSummary | null,
    /** 上一次会话因过期/吊销而失效（登录页据此提示）。 */
    expired: false,
  }),
  getters: {
    access(state): Access | null {
      return state.me ? accessFromMe(state.me) : null;
    },
    user(state): UserSummary | null {
      return state.me?.user ?? null;
    },
    mustChangePassword(state): boolean {
      return state.me?.user.must_change_password === true;
    },
  },
  actions: {
    /** 读取当前身份。401 = 匿名；其他错误（网络/5xx）向上抛出，由调用方显示失败态而不是误判为已退出。 */
    async restore(): Promise<void> {
      try {
        const me = await api().me();
        this.applyMe(me);
      } catch (e) {
        if (isApiError(e) && e.status === 401) {
          this.clear();
          return;
        }
        throw e;
      }
    },
    applyMe(me: Me): void {
      if (me.realm !== 'admin') {
        // 防御：Admin 入口只接受 admin realm 的会话。
        this.clear();
        return;
      }
      this.me = me;
      this.status = 'authed';
      this.expired = false;
      if (me.csrf_token) this.csrf = me.csrf_token;
    },
    async login(email: string, secret: string): Promise<LoginOutcome> {
      const res = await api().login(email, secret);
      if (res.status === 'totp_required') {
        if (!res.mfa_token) throw new Error('login: totp_required without mfa_token');
        this.mfa = { token: res.mfa_token, expiresAt: Date.now() + MFA_TTL_MS };
        return 'totp_required';
      }
      return this.finishLogin(res.csrf_token, res.status === 'must_change_password' ? 'must_change' : 'ok');
    },
    async verifyTotp(code: string): Promise<LoginOutcome> {
      const ch = this.mfa;
      if (!ch) throw new Error('verifyTotp: no pending challenge');
      const res = await api().verifyTotp(ch.token, code.trim());
      this.mfa = null;
      return this.finishLogin(res.csrf_token, res.status === 'must_change_password' ? 'must_change' : 'ok');
    },
    async finishLogin(csrf: string | undefined, outcome: LoginOutcome): Promise<LoginOutcome> {
      if (csrf) this.csrf = csrf;
      this.mfa = null;
      await this.restore(); // 以服务端 /auth/me 为准（权限、能力清单、运行模式）
      if (this.status !== 'authed') throw new Error('login: session was not established');
      return outcome;
    },
    /** 修改口令：成功后服务端吊销全部会话（含当前），本地状态随之清空。 */
    async changePassword(current: string, next: string): Promise<void> {
      await api().changePassword(current, next);
      this.clear();
    },
    cancelMfa(): void {
      this.mfa = null;
    },
    /** 默认租户（代管目标）：仅在有 tenant:read 时尝试；失败不影响登录，横幅退回通用文案。 */
    async resolveActingTenant(): Promise<void> {
      this.actingTenant = null;
      if (!this.me?.permissions.includes('tenant:read')) return;
      try {
        const list = await api().listTenants();
        const t = list.items.find((x) => x.is_default === true);
        if (t) this.actingTenant = { uid: t.uid, name: t.name, slug: t.slug, is_default: true };
      } catch {
        /* 横幅退回通用文案 */
      }
    },
    async logout(): Promise<void> {
      try {
        await api().logout();
      } catch {
        /* 服务端不可达也要清理本地状态；会话终将过期 */
      }
      this.clear();
    },
    /** 会话失效（401）：清空本地身份；登录页提示「会话已过期」。 */
    expire(): void {
      if (this.status === 'authed') this.expired = true;
      this.clearIdentity();
    },
    clearIdentity(): void {
      this.me = null;
      this.csrf = null;
      this.mfa = null;
      this.actingTenant = null;
      this.status = 'anon';
    },
    clear(): void {
      this.clearIdentity();
      this.expired = false;
    },
  },
});
