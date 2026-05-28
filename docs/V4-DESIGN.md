# MeshCDN V4 设计文档

**版本**: v0.4 (2026-04-27) — 命令系统闭环
**状态**: 设计完整，可开始实现
**目的**: V4 重写的"宪法"——所有实现决策的最终依据

**v0.4 主要变更**：
- 新增 **§A 三套系统总览**（骨架/肌肉/血液），作为整体架构图
- 命令系统正式定型：严格四段式 + 镜像对称 + 对象/绑定双层抽象
- 新增 §8 完整命令几何（A/B/C 三类命令、主键、合并规则、端口协议全局统一）
- 数据库 schema 大纲、错误码分类、二次确认 UX 一并钉死

---

# §A 三套系统总览

V4 的整体架构按**静态/半静态/动态**分为三套系统。每套系统对应代码与文件的不同生命周期。

## A.1 骨架（Skeleton）

磁盘上躺着不动的东西。**双目录方案**。

```
/etc/meshcdn/
├── persistent/                       ← 跨升级保留
│   ├── identity.json                 节点身份 + bot token + group_id
│   ├── peers.json                    集群节点 IP 列表
│   ├── certs/                        所有证书 + manifest.json
│   └── snapshot.cmd                  当前生效配置导出（含版本号头）
│
└── runtime/                          ← 升级时全删，由 persistent 重建
    ├── config.db                     SQLite 主数据库
    ├── logs.db                       访问日志
    ├── nginx/                        生成的 nginx 配置
    ├── welcome/                      默认页面（每次升级覆盖）
    ├── challenges/                   ACME 验证临时文件
    ├── cache/                        nginx 缓存
    ├── logs/                         原始访问日志
    └── tmp/                          通用临时文件
```

升级流程的物理操作：**删 runtime/ 整个目录、停服务、替换二进制、启动服务、agent 自动从 persistent 重建 runtime**。

如果将来 V5 大改架构，只要 V5 还认识这 4 类 persistent 文件，就能从 V4 平滑升级。

源码组织（项目目录树）：

```
meshcdn/
├── cmd/cdn-agent/main.go             程序入口
├── internal/                         所有业务逻辑（Go 的 internal 包对外不可导入）
│   ├── identity/                     管 persistent/identity.json
│   ├── peers/                        管 persistent/peers.json
│   ├── snapshot/                     管 persistent/snapshot.cmd
│   ├── db/                           管 runtime/config.db
│   ├── command/                      ★肌肉系统
│   │   ├── types.go                  Command / Handler / Effects / Rule / 错误码
│   │   ├── parser.go                 严格四段式解析
│   │   ├── executor.go               批处理事务
│   │   └── handlers/                 每种 type 一个 handler
│   ├── nginx/                        runtime/nginx/* 生成
│   ├── cert/                         证书子系统
│   │   ├── store.go                  certs/ + manifest.json
│   │   ├── selector.go               §3.6 选择算法
│   │   ├── acme/                     ACME 客户端
│   │   └── renew/                    ★续签循环
│   ├── mesh/                         ★血液循环系统
│   │   ├── server.go                 HTTP+JSON 端点
│   │   ├── heartbeat.go              心跳每分钟
│   │   ├── pull.go                   snapshot 拉模型
│   │   └── events.go                 双流的事件流
│   ├── bot/                          Telegram 接口
│   ├── cli/                          cdn-agent exec
│   └── version/                      版本 + 嵌入源码
├── source/                           构建时填充的源码副本
├── scripts/install.sh, bootstrap.sh
├── docs/V4-DESIGN.md
├── go.mod, go.sum, Makefile
```

**包边界 = 物理边界**：`internal/identity/` 管 `persistent/identity.json`，`internal/snapshot/` 管 `persistent/snapshot.cmd`。修改某文件格式只动一个包。

## A.2 肌肉（Muscle）

`internal/command/` 包及其 handlers。**输入到行为的转换规则**。

核心是**严格四段式**：

```
verb  type   scope   params
─────────────────────────────
/w   domain   ...     ...
/d   domain   ...     ...
/v   domain   ...     ...
```

新增功能 = 新增 `handlers/<name>.go`，实现 `Handler` 接口，注册到 Registry。**升级时这个包必然变化（新功能），但接口不变**——这是"升级不影响命令逻辑"的物理体现。

详见 §8。

## A.3 血液（Blood）

运行时的三个自动循环。**持续流动**。

```
循环 1 ─ 心跳（每 1 分钟）
  internal/mesh/heartbeat.go
  ping 所有 peer，交换 (config_version, program_version, peer_count)
  测得 RTT 写入本地 peer 表
  发现版本落后 → 触发拉同步任务

循环 2 ─ 证书扫描（每 6 小时）
  internal/cert/renew/scanner.go
  扫所有证书，剩余 < 3 天的进入续签流程（§3.7）

循环 3 ─ 事件流处理（持续）
  internal/mesh/events.go
  接收双流的事件消息（告警、challenge 共享、bot 转移通知）
  分发给对应处理器
```

**循环之间不直接调用，只通过 channel + db 沟通**。任何一个循环挂了或卡了，其他循环不受影响。

---

# §0 设计哲学

V4 在 v3.1 基础上保留四条核心原则，新增两条：

1. **节点平权**：所有节点配置一致。所谓"主节点"只是当前对接 Bot 命令的那个，无任何独特之处。

2. **命令即配置**：配置不存在另一种格式。配置就是命令序列。导出即模板，重放即恢复。

3. **精度即优先级**：所有匹配类规则按精度自动排序，无需手工指定权重。

4. **心跳同步**：节点间相互探测、相互核对、配置自动收敛。

5. **事务一致性**：批处理是原子单位，版本号只承诺成功命令，失败命令从协议层不存在。

6. **程序的边界 = 信息边界**：程序负责告知，人负责决定。告警是义务，兜底不是义务。例外仅限"维持系统可启动 / 可通信"的最低限度。

V4 显式放弃 v3.1 的概念：
- 规则模板（ruleset）→ 用对象/绑定双层抽象替代（§8.4）
- 域名组 → 批量命令处理批量场景
- 三流版本号（cluster/routing/policy）→ 合并为单一 `config_version`
- "选定后不自动切换" → 与精度优先冲突，废除
- IP 证书与域名证书的差异化 → IP = 域名，统一规则
- 过度兜底 → 升级 boot 阶段的自签检查、agent 首次启动的补签都删除

---

# §1 同步机制

## 1.1 心跳协议

每个节点每分钟向所有 peer 发送心跳。心跳交换：

```
config_version    : int    当前配置版本号
program_version   : str    当前程序版本
peer_known_count  : int    本节点已知的 peer 总数
```

副作用：测得 RTT 写本地 peer 表。

## 1.2 主动推送

Bot 节点执行批处理后立即向所有 peer 发送 `notify-version`（仅含新版本号）。peer 收到后立即对比本地版本，落后则拉。

`/sync` 是手动触发主动推送。

主动推送和心跳兜底**并行存在**——主动推送可能丢包，心跳是最终一致性保证。

## 1.3 拉模型（snapshot）

落后节点决定拉时：
1. 在本地 peer 表筛选版本号 ≥ 自己 +1 的 peer
2. 在该集合内挑选 RTT 最低者
3. 拉完整配置 snapshot（命令列表 + 目标版本号）
4. 本地清空 + 重放 + 提号（同 SQLite 事务）

落后节点不依赖 bot 节点，拉自任一更新的 peer 即可。

## 1.4 版本号机制

`config_version` 是单调递增整数。每个批处理（含单条命令的特例）+1。落盘和提号在同一事务：

```sql
BEGIN;
  -- apply commands to db
  UPDATE cluster_meta SET config_version = config_version + 1;
COMMIT;
```

## 1.5 程序升级带宽探测

配置 snapshot 数据量小，RTT 低基本=快。程序升级（5MB binary）值得做小探针：升级时向候选 peer 发 1MB 探针 HEAD 请求，挑带宽最高者下载。

## 1.6 双流协议

V4 mesh 协议分两类消息：

- **配置同步类**：进入版本号流，必达，最终一致
- **事件通知类**：不进版本号流，瞬时消息（告警、challenge 共享、bot 转移）

事件通知类丢失可接受，由更高层机制兜底。

---

# §2 事务语义

## 2.1 批处理为原子单位

用户一次发送 N 条命令（换行分隔），bot 顺序执行：
- 失败命令**跳过继续**
- 全部跑完后，**只有成功命令进入新版本号**
- 失败信息以执行报告形式回报用户
- 一批 N 条 = 一个版本号增量

## 2.2 失败命令不进协议层

其他节点拉到的版本号永远只承诺"已确认成功"的命令切片。

## 2.3 一致性保证

V4 是**最终一致性**系统。同步延迟正常 <1 秒（主动推送），异常 <2 分钟（心跳兜底两轮）。

---

# §3 证书机制

## 3.1 设计纲领

- **IP 和域名无差异**——同一套命令、申请、同步、选择算法
- **三种来源**：LE / 上传 / 自签——元数据标签，处理流程相同
- **来源即精度**：LE > 上传 > 自签

事实前提：Let's Encrypt 自 2025 年 7 月正式签发 IP 证书（短期 6 天）。V4 设计基于此前提。

## 3.2 申请节点选择

```
candidates(D) = [节点 P : D 的 DNS 解析包含 P 的 IP]
responsible(D) = candidates(D)[hash(D) % len(candidates(D))]
```

DNS 圈定候选集，hash 在候选集内分配。DNS 变动 → 候选集变动 → hash 重新分配。

## 3.3 HTTP-01 验证（V4 默认）

负责节点 X 申请前，把 ACME challenge token 通过事件流广播给候选集内所有节点。所有候选节点的 nginx 从共享 challenge 目录读。

## 3.4 DNS-01 验证（V4.1 目标）

V4 不实现。

## 3.5 证书同步

LE / 上传证书入库后走配置同步流广播全集群。**自签不参与同步**。

## 3.6 证书选择算法

```
对域名 / IP 端点 X 选择当前生效证书：
  candidates = 所有覆盖 X 的、未过期的证书
  
  排序（高到低）：
    1. 来源：LE > 上传 > 自签
    2. 同来源内：到期时间最晚者优先
  
  选定后行为：
    - 当前选定未过期前不切换
    - 当前选定过期 / 失效后立即按算法重选
```

## 3.7 续签流程

每 6 小时扫描，剩余 < 3 天的当前选定证书 X：

```
步骤 0：判断
  X.来源 ∈ {LE, 自签}  → 步骤 1
  X.来源 == 上传        → 步骤 2

步骤 1：自动续签
  LE → responsible 节点走 ACME（异步任务）
  自签 → 本机重生成
  成功 → 替换 X
  失败 → 步骤 2

步骤 2：找替代
  candidates 中 not_after > X.not_after 的非自签证书
  找到 → 切换
  找不到 → 步骤 3

步骤 3：告警
  事件流推送到 Telegram，含 ACME 原始错误信息
  24 小时去重

——无步骤 4。证书过期由 §3.6 选择算法自然处理。
```

实现：异步任务队列（进程内内存队列），不持久化。重启后下次扫描自动重新发现。

## 3.8 自签证书

- **生成时机**：install.sh 阶段。**install.sh 是唯一责任主体**——agent 启动时不补签
- **有效期**：100 年（一次性）
- **续签**：3 天阈值触发时本机重生成
- **同步**：不参与
- **存储**：`/etc/meshcdn/persistent/certs/`，仅来源标签不同
- **优先级**：最低，作为兜底

## 3.9 证书目录与命名

按内容指纹命名：

```
/etc/meshcdn/persistent/certs/<sha256-prefix>.crt
/etc/meshcdn/persistent/certs/<sha256-prefix>.key
/etc/meshcdn/persistent/certs/manifest.json
```

`manifest.json` 是元数据索引：

```json
{
  "certificates": {
    "<sha256-prefix>": {
      "subject": "a.com",
      "san": ["a.com"],
      "source": "le|upload|self",
      "issuer": "Let's Encrypt R3",
      "not_before": "...",
      "not_after": "...",
      "fingerprint_sha256": "...",
      "selected_for": ["a.com"]
    }
  }
}
```

---

# §4 升级与持久化

## 4.1 跨升级保留清单

仅 4 项：

| 路径 | 内容 |
|---|---|
| `persistent/identity.json` | 节点身份 |
| `persistent/peers.json` | 集群节点 IP 列表 |
| `persistent/certs/` | 所有证书 + manifest |
| `persistent/snapshot.cmd` | 当前配置导出 |

`runtime/` 整个目录每次升级删除重建。

## 4.2 snapshot 文件格式

```
# version: 53
# exported: 2026-04-27T12:34:56Z
# program: v4.0.0
/w domain https://a.com:443 https://1.2.3.4:443
/w cache  img-7d            patterns=*.jpg,*.png ttl=604800
/w bind   a.com             cache:img-7d
/w ssl    a.com             -
...
```

每次成功批处理后重新导出覆盖（先写临时文件再 rename）。

## 4.3 升级 boot 流程

```
1. 读取 persistent/ 4 项
2. 从 snapshot 文件头恢复 config_version 到内存
3. 重建数据库 schema
4. 重放 snapshot 命令到 db
5. 生成 nginx 配置 + 启动 OpenResty
6. 加入心跳网络
7. 与邻居对比版本号，必要时拉最新配置
```

无"自签检查兜底"步骤。

## 4.4 发布物结构与源码自包含

**任何已发布的二进制都必须能恢复其对应的源码**。

实现：双保险

1. release tarball 含源码副本（独立目录）
2. 二进制内嵌入源码（Go embed）
3. commit hash 编入二进制

发布物结构：

```
meshcdn-v4.0.0-linux-amd64.tar.gz
├── cdn-agent               二进制（含嵌入源码）
├── install.sh
├── source/                 平铺源码（不含 .git/）
└── VERSION                 commit hash + build time
```

新增命令：

```bash
cdn-agent --version           # 输出含 commit hash
cdn-agent dump-source         # 默认输出到 ./meshcdn-source-<commit>/
```

构建流程纪律：CI 把"打包源码 + 嵌入二进制"作为发布前的强制步骤。**没有源码自包含的二进制不允许 release**。

---

# §5 节点管理

## 5.1 认亲

任意 peer 都可接受认亲。新节点 N 向已有节点 P 认亲：

1. N 提交 `secret = sha256(group_id + bot_token)`
2. P 验证后发起一次"新增 peer"配置变更（进入版本号同步流）
3. 集群内所有节点按版本号同步收敛
4. P 返回当前 snapshot 给 N
5. N 重放 snapshot + 加入心跳网络

## 5.2 Peer 名单纳入配置流

peer 加入 / 退出 = 一条协议命令 `/internal/peer-add` / `/internal/peer-remove`，+1 版本号。

## 5.3 Bot 节点确定

- 第一个安装的节点默认 bot
- `/target <ip>` 转移 bot 权限
- bot 失联：**纯手动接管**。SSH 到任一存活节点执行 takeover

不实现自动 drift / 自动告警 / split-brain 防护。

---

# §6 IP 端点访问行为

按 **IP = 域名** 原则：

```
访问 https://1.2.3.4:443
  ↓
1.2.3.4 是否被 /w domain 显式注册过？
  ├── 否 → default server + 自签 + CDN 默认页面（200）
  └── 是 → 走该 IP 端点配置（按 §3.6 选证书）
```

---

# §7 CDN 默认页面

- 内置静态 HTML，install.sh 阶段释放到 `runtime/welcome/index.html`
- 不可自定义（V4 第一版）
- 不同步
- 不在跨升级保留清单（每次升级覆盖）
- 状态码 200

---

# §8 命令系统（肌肉层）

## 8.1 严格四段式

所有命令**严格四段**：

```
/<verb> <type> <scope> <params>
```

- 每段必须存在，空位用 `-` 占位
- 解析器永远期望四段，无例外

格式：

```
verb     ::= w | d | v
type     ::= 资源类型字符串（domain / ssl / cache / defense / bind / ...）
scope    ::= 取决于 type
params   ::= 取决于 type；可为 - 或 key=value 序列
```

`-` 在所有字段统一为"兜底/默认/无限定"语义。

## 8.2 镜像对称（w/d）

w 和 d 是镜像操作。**导出 = 把内存状态 dump 为 w 命令序列；删除 = 把对应 w 改为 d**。

```
写：/w cache img-7d patterns=*.jpg ttl=604800
删：/d cache img-7d patterns=*.jpg ttl=604800
```

w/d 共享 Parse 和 Validate 路径，仅 Execute 时分叉。

**主键判定**：每个 type 自定义"哪些字段构成主键"。相同主键的两条 w 视为同一规则的两次写入（覆盖语义）。d 命令按主键匹配；params 中的非主键字段在 d 命令中可写可不写，仅做形式校验。

## 8.3 命令分类（A/B/C 三类 + 集群元数据 + 系统动作）

### A 类：直接命令（scope = 真实业务对象）

```
/w domain   <host:port>       <origin>            写入域名
/w ssl      <域名/IP>         <动作或 ->          申请证书
/w sslfile  <域名/IP>         <选项或 ->          上传证书
```

### B 类：对象命令（scope = 对象名，无作用域）

```
/w cache    <对象名>          <key=value 单行>    定义缓存对象
/w defense  <对象名>          <key=value 单行>    定义防御对象
/w redirect <对象名>          <key=value 单行>    定义重定向对象
/w header   <对象名>          <key=value 单行>    定义头部对象
```

### C 类：绑定命令

```
/w bind     <域名/IP>         <对象类型>:<对象名>
```

### 集群元数据查询（仅 v）

```
/v export   -                 -                   导出全集群配置
/v status   -                 -                   本节点状态
/v stats    -                 -                   流量统计
/v nodes    [- | <peer-ip>]   -                   peer 列表/详情
```

### 系统动作命令（独立动词）

```
/sync                                              主动触发同步
/target  <peer-ip>                                 转移 bot 角色
/upgrade                                           触发集群升级
/menu                                              显示菜单
/help                                              显示帮助
/confirm <ID>                                      二次确认（前一条危险操作）
```

## 8.4 对象/绑定模式（替代 v3.1 的 ruleset）

定义对象（B 类）和绑定关系（C 类）解耦：

```
/w cache img-7d  patterns=*.jpg,*.png ttl=604800
/w cache html-1h patterns=*.html      ttl=3600
/w bind a.com    cache:img-7d
/w bind a.com    cache:html-1h
```

- 一个对象可被多个域名绑定
- 一个域名可绑定多个对象
- 删除被引用的对象 → 警告 + `/confirm` 二次确认级联删除

## 8.5 多绑定的精度优先匹配

同一域名绑定多个对象时，按精度规则匹配：

```
A 模板: patterns=*.jpg,*.png  ttl=604800
B 模板: patterns=zhangsan.jpg ttl=86400

a.com 绑定 A 和 B：
  请求 zhangsan.jpg  → B 胜出（精确匹配）→ 缓存 1 天
  请求 lisi.jpg     → A 胜出（通配）  → 缓存 7 天
```

精度规则：精确 > 前缀 > 正则 > 通配；CIDR 前缀长度越长精度越高。

**实现委托给 nginx**：cdn-agent 把所有 pattern 按精度顺序生成 nginx location 块（精确匹配 `location =`、前缀 `location ^~`、正则 `location ~*`、通配兜底），nginx 自身的匹配引擎完成精度优先匹配。**cdn-agent 不写运行时匹配引擎**。

## 8.6 同精度合并规则

同精度 pattern 在不同对象里冲突时，**取"更严格"的字段值**：

| 字段类型 | 合并规则 |
|---|---|
| TTL | 取最小值 |
| HSTS / 其他布尔（启用即更严格） | OR |
| Action（block/allow） | block 胜出 |
| 其他严格性字段 | 严格者胜出 |

不报错、不让用户挑、不按 bind 顺序——**直接合并**。

## 8.7 端口协议全局统一

每个端口在全集群有且仅有一个协议绑定。

实现：runtime/config.db 维护一张 `port_protocols(port, protocol)` 表。每条 `/w domain` 命令执行时根据 host:port 段隐式更新此表。

冲突处理：

```
/w domain http://b.com:443 ...    # 但 443 已绑定为 https
   ↓
返回 ErrPortConflict，给出 PendingConfirmation
   ↓
"⚠️ 端口 443 已绑定为 https。回复 /confirm <ID> 强制改为 http"
   ↓
用户回 /confirm <ID> → 端口切换 → 全集群同步
```

`/v export` 不显式输出端口绑定（隐式从 domain 命令重建）。

## 8.8 二次确认 UX（PendingConfirmation）

触发条件：
- 端口协议冲突
- 级联删除（删除有引用的对象）
- 未来可能的其他危险操作

机制：
- 危险命令执行前生成 `PendingConfirmation`，含 short ID
- 存储在**进程内内存**（不持久化，重启丢失）
- 通知用户："⚠️ 原因。回复 `/confirm <ID>` 继续"
- TTL = 5 分钟，过期自动清除
- 用户回 `/confirm <ID>` → 取出原命令，标记"已确认"再执行
- 重启 / 5 分钟过期 → 用户重发命令即可

CLI 端：

```bash
cdn-agent exec "/w domain http://b.com:443 ..."
# Error: PORT_CONFLICT — 443 currently bound to https. Use --force to override.

cdn-agent exec --force "/w domain http://b.com:443 ..."
# ✅ 端口 443 已切换为 http
```

---

# §9 数据库 Schema 大纲

`runtime/config.db` 主要表：

```sql
-- 元数据
CREATE TABLE cluster_meta (
    config_version INTEGER NOT NULL DEFAULT 0,
    bot_node_ip    TEXT,
    program_version TEXT
);

-- A 类规则（直接命令）
CREATE TABLE domains (
    id INTEGER PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,    -- "https://a.com:443"
    origin TEXT NOT NULL,          -- "https://1.2.3.4:443" or "-"
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- 端口协议全局表
CREATE TABLE port_protocols (
    port INTEGER PRIMARY KEY,
    protocol TEXT NOT NULL
);

-- B 类规则对象（统一表，type 字段区分）
CREATE TABLE rule_objects (
    id INTEGER PRIMARY KEY,
    type TEXT NOT NULL,            -- "cache" / "defense" / "redirect" / "header"
    name TEXT NOT NULL,            -- 对象名
    params_text TEXT NOT NULL,     -- 原始 key=value 文本
    parsed_json TEXT NOT NULL,     -- handler 解析后的结构化 JSON（供 nginx 生成器读）
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(type, name)
);

-- C 类绑定关系
CREATE TABLE bindings (
    id INTEGER PRIMARY KEY,
    scope TEXT NOT NULL,           -- "a.com" 或 IP
    object_type TEXT NOT NULL,
    object_name TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(scope, object_type, object_name),
    FOREIGN KEY (object_type, object_name) REFERENCES rule_objects(type, name)
);

-- 证书（与 manifest.json 镜像，db 是查询友好副本）
CREATE TABLE certificates (
    fingerprint_prefix TEXT PRIMARY KEY,
    subject TEXT NOT NULL,
    source TEXT NOT NULL,          -- "le" / "upload" / "self"
    issuer TEXT,
    not_before TIMESTAMP,
    not_after TIMESTAMP,
    san_json TEXT NOT NULL         -- ["a.com", "*.a.com"]
);

-- Peer 列表（与 peers.json 镜像）
CREATE TABLE peers (
    ip TEXT PRIMARY KEY,
    join_order INTEGER NOT NULL,
    last_seen_rtt_ms INTEGER,
    last_seen_at TIMESTAMP
);
```

`runtime/logs.db` 是访问日志的独立数据库，schema 在第 7 步实现时再细化。

---

# §10 错误码分类

所有命令系统错误返回 `CommandError` 接口，含 `Code()` 短码：

| Code | 含义 |
|---|---|
| `BAD_FORMAT` | 命令形态错误（非四段、verb 非法） |
| `UNKNOWN_TYPE` | type 不在 Registry |
| `BAD_PARAMS` | params 解析失败或值非法 |
| `NOT_FOUND` | /d 不存在的规则（信息性，不一定致命） |
| `CONFIRM_REQUIRED` | 命令被 hold，等待 /confirm |
| `CONFIRM_EXPIRED` | /confirm 来得太晚 |
| `CONFIRM_UNKNOWN` | /confirm 的 ID 未知 |
| `PORT_CONFLICT` | 端口协议冲突无 confirm |
| `CASCADE_BLOCKED` | 删除有引用的对象 |
| `INTERNAL` | bug |

bot/cli 层根据 Code 决定 UX——Telegram 端可以 emoji + 友好提示，CLI 端可以非零退出码 + stderr 输出。

---

# §11 与 v3.1 的差异总览

| 维度 | v3.1 | V4 |
|---|---|---|
| 命令格式 | 各 type 段数不同 | 严格四段式（verb + type + scope + params） |
| 命令分类 | 平铺 | A/B/C 三类 + 集群元数据 + 系统动作 |
| 规则复用 | ruleset 模板 | 对象 + 绑定双层抽象 |
| 精度优先 | 部分支持（域名层） | 全面支持（域名/path/IP），引擎委托 nginx |
| 端口协议 | 每条 domain 自带 | 全集群统一表 + 二次确认 |
| 版本号 | 三流 | 单流 config_version |
| 同步语义 | delta 广播 | snapshot 拉模型 |
| 失败命令 | 进入广播流 | 不进协议层 |
| 批处理 | 单命令为单位 | 批为原子单位 |
| ruleset | 一等公民 | 删除 |
| IP vs 域名 | 差异化 | 完全统一 |
| 证书来源 | LE + 自签 IP（特殊） + 上传 | LE + 上传 + 自签（统一） |
| 证书选择 | 选定后不切换 | 按精度选最优 + 当前未过期不切换 + 过期立即重选 |
| 自签 | 6 天 IP 证书 | install.sh 生成 100 年 |
| 兜底哲学 | 多层防御 | 信息边界（程序告知，人决定） |
| 文件命名 | 按域名/IP | 按内容指纹 + manifest |
| 跨升级保留 | 4 项 + db | 4 项（db 重建） |
| 持久化分层 | 多目录 | 双目录（persistent/runtime） |
| Bot 失联 | 自动 drift | 纯手动接管 |
| Peer 名单 | 独立 Addition 广播 | 纳入配置流 |
| 源码可恢复 | 不保证 | 强制（commit hash + embed） |

---

# §12 实现优先顺序

按依赖顺序：

| 步 | 内容 | 时间 | 里程碑 |
|---|---|---|---|
| 0 | 项目骨架（目录树 + Makefile + version + dump-source） | 半天 | `cdn-agent --version` 工作 |
| 1 | DB schema + command 解析 + 第一个 handler（domain）+ CLI exec | 2-3 天 | `cdn-agent exec /w domain ...` 写入 + 查回 |
| 2 | OpenResty 生成器 + install.sh + 自签兜底 | 3-4 天 | 单机能反代真实流量 |
| 3 | 证书子系统完整版（LE + 上传 + 续签） | 4-5 天 | LE 证书自动签发、续期 |
| 4 | Mesh 协议骨架 + 双节点同步 | 5-7 天 | 两节点配置秒级同步 |
| 5 | 批处理事务 + snapshot 文件 + 集群升级 | 3-4 天 | 滚动升级无丢失 |
| 6 | 对象/绑定 + 精度生成器 + 端口协议表 | 3-4 天 | cache/defense 对象工作 |
| 7 | Telegram bot 接口 | 2-3 天 | v3.1 命令在 V4 全跑通 |

第 3 步完成（约 2 周）有可独立部署的单节点 V4。第 6 步完成时命令系统全部实现，第 7 步收尾。

---

# 附录 A：变更历史

- **v0.1** (2026-04-27)：初版，协议层完整收口
- **v0.2** (2026-04-27)：证书机制 + IP 端点 + 默认页面闭环
- **v0.3** (2026-04-27)：设计哲学新增第 6 条；续签流程简化；删除两处过度兜底；新增源码自包含
- **v0.4** (2026-04-27)：新增三套系统总览（骨架/肌肉/血液）；命令系统正式定型（严格四段 + 镜像对称 + 对象/绑定 + 精度优先 + 端口协议全局统一 + 二次确认 UX）；数据库 schema 大纲；错误码分类
