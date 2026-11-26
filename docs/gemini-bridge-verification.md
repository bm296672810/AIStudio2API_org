# Gemini Cookie 桥接对接与验证指南

本文面向运维与后续接手开发者，说明 AIStudio2API → Gemini-Web2API 的 Cookie 桥接如何联调、验证与排障。架构与协议细节见 [cookie-bridge-design.md](cookie-bridge-design.md)，配置项见 [development.md](development.md)。

当前实现覆盖设计中的 M1–M3：进程级常驻桥接器、定时单向推送、配对接口引导与续签凭据持久化。**没有管理端界面**（M4 未实现），全部通过日志与文件观察。

## 1. 前置条件

| 项 | 要求 |
| --- | --- |
| AIStudio2API 版本 | 含桥接代码的版本（`v0.3.15-1.0` 起），管理页侧边栏版本号可确认；旧版本配置不生效 |
| Gemini-Web2API | 已部署运行，支持配对上传 `POST /api/pairing/accounts` |
| 出口 | 两端同一公网出口。桥接透传账户代理（为空时回落全局 `PROXY`），两端出口不一致易触发 Google 验证 |
| WAA 后端 | 无要求。桥接只读账户目录，SpiderMonkey 后端同样可用 |

## 2. 冒烟验证：首推成功

1. 在 Gemini 管理端「远程添加 / 导入账号」生成配对令牌（10 分钟有效）。
2. AIStudio 的 `.env` 写入：

   ```ini
   GEMINI_BRIDGE_ENABLED=true
   GEMINI_BRIDGE_ORIGIN=http://127.0.0.1:8007
   GEMINI_BRIDGE_INTERVAL=1m
   GEMINI_BRIDGE_PAIRING_TOKEN=<刚生成的令牌>
   ```

   先用 `1m` 间隔做快速验证，验证完改回 `10m`。**桥接配置在管理进程启动时读取，改完必须重启整个程序**，而不是点"重启生成服务"。

3. 启动 AIStudio，在日志页（source=service）观察：
   - `Gemini Cookie 桥接已启动`，字段含 `origin`、`interval`；
   - 每个合格账户出现 `桥接账户已推送`，字段含 `account`、`gemini_id`、`device_bound`。

4. 检查产物：
   - `runtime/gemini-bridge.json` 存在，含各账户 `gemini_id`、`renewal_token`、`last_status: "ok"`、`last_push_at`，文件权限 0600；
   - Gemini 账户列表出现对应账号，ID 为邮箱规整后的形式（`name@gmail.com` → `name-gmail-com`）。

5. 用 Gemini 发一个真实请求，确认账号能出结果，而不只是导入成功。

首传成功后把 `GEMINI_BRIDGE_PAIRING_TOKEN` 清空：桥接已换取长期续签凭据，后续不再需要配对令牌。

## 3. 持续同步验证

- 等一个间隔，观察日志是否周期性出现 `桥接账户已推送`。
- Cookie 未变化时桥接器按指纹去重**静默跳过**（无推送日志），这是预期行为，不是卡住。
- 登录态在 AIStudio 侧更新（重新登录、Cookie 轮换）后，下个 tick 应再次推送。

## 4. 故障自愈验证

| 演练 | 操作 | 预期 |
| --- | --- | --- |
| 对端不可用 | 停掉 Gemini，等一次 tick | 日志 `桥接推送失败，安排退避重试`，`backoff` 从 `30s` 指数递增到上限 `10m`；重启 Gemini 后到点自动恢复，续签凭据保留 |
| 副本失效 | 改坏 Gemini 中该账号的 `__Secure-1PSIDTS`，使其进入 auth_required | 下个 tick 推送新鲜 Cookie，Gemini 热重启后账号恢复可用 |
| 令牌失效 | 删除 Gemini 侧该账号，观察下一次续推 | 日志 `桥接令牌失效，等待重新配对`；`gemini-bridge.json` 中该账户 `renewal_token` 清空、`last_status` 变为 `pairing_required`；重新生成配对令牌并重启即可恢复引导 |

## 5. 边界行为核对

| 场景 | 预期 |
| --- | --- |
| AIStudio 账户停用 | 跳过推送，Gemini 侧保留 |
| 缺 `SAPISID` 或 `__Secure-1PSID` | `桥接跳过缺少核心 Cookie 的账户`，不推送 |
| Cookie 未变化 | 去重跳过，不触发 Gemini 热重启 |
| 设备绑定账户（含 `__Secure-1PSIDRTS`） | 推送日志 `device_bound=true`，按间隔一半推送 |
| 生成服务停止 / 启动 | 桥接不受影响（进程级常驻） |
| 远程 origin 使用 HTTP | 配置校验拒绝，非本机地址必须 HTTPS |
| `GEMINI_BRIDGE_INCLUDE` 非空 | 仅同步名单内邮箱 |

## 6. 关键文件与日志速查

- 令牌与状态：`runtime/gemini-bridge.json`，字段 `origin`、`accounts[email].{gemini_id, renewal_token, device_bound, last_push_at, last_status, last_error}`。
- 启动日志：`Gemini Cookie 桥接已启动` / `Gemini Cookie 桥接已停止`。
- 成功日志：`桥接账户已推送`。
- 等待配对：`桥接账户等待配对令牌`；令牌失效：`桥接令牌失效，等待重新配对`。
- 状态被拒：`桥接账户状态被对端拒绝，登录态变化前跳过`。
- 退避：`桥接推送失败，安排退避重试`，字段 `backoff`。

## 7. 常见误区

1. **改 .env 只重启了生成服务**：桥接是管理进程级组件，必须整个程序退出重开。
2. **把去重跳过当成故障**：Cookie 未变化时不推送是设计行为。
3. **两端出口 IP 不一致**：桥接透传代理配置，请保持同一出口，否则 Gemini 侧账号易被踢。
4. **用旧版本验证**：桥接代码随 `v0.3.15-1.0` 引入，先核对版本号再排查配置。
