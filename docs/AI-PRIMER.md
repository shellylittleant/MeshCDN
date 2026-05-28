# MeshCDN V4 — AI 读本

**Read this first. Everything else is implementation detail.**

> 读完这份文件，AI 工具就能进入项目状态。每次会话开始默认读这一份。
> 真正的设计文档是 `V4-DESIGN.md`，本文件是它的精简提取。

---

## 项目是什么

MeshCDN 是一个**自建分布式 CDN**。多台 VPS 节点通过 mesh 协议组成集群，由 Telegram bot 控制。每个节点跑 OpenResty + cdn-agent。Go 单二进制，约 5MB。

---

## 六条设计哲学（任何决策都要回到这里）

1. **节点平权** — 所有节点配置一致。"主节点"只是当前对接 bot 的那个。
2. **命令即配置** — 配置就是命令序列。导出即模板，重放即恢复。
3. **精度即优先级** — 所有匹配类规则按精度自动排序，无需手工指定权重。
4. **心跳同步** — 节点相互探测、相互核对、配置自动收敛。
5. **事务一致性** — 批处理是原子单位，版本号只承诺成功命令，失败命令从协议层不存在。
6. **程序的边界 = 信息边界** — 程序负责告知，人负责决定。告警是义务，兜底不是义务。例外仅限"维持系统可启动 / 可通信"。

---

## 三套系统（架构骨架）

```
骨架 Skeleton  →  /etc/meshcdn/persistent/  +  /etc/meshcdn/runtime/
                  双目录方案。升级时 runtime/ 全删，persistent/ 保留。

肌肉 Muscle    →  internal/command/
                  严格四段式命令。所有外部输入都经此处。

血液 Blood     →  internal/mesh/  +  internal/cert/renew/
                  三个后台循环：心跳 / 证书扫描 / 事件流。
```

---

## 命令系统（必背）

### 严格四段式

```
/<verb> <type> <scope> <params>
```

每段必须存在。空位用 `-` 占位。无例外。

- **verb**: `w`(写) / `d`(删) / `v`(查)
- **type**: `domain` / `ssl` / `sslfile` / `cache` / `defense` / `redirect` / `header` / `bind` / 集群元数据 / ...
- **scope**: 取决于 type（域名 / IP / 对象名 / `-`）
- **params**: key=value 序列 或 `-`

### w/d 镜像对称

```
写：/w cache img-7d patterns=*.jpg ttl=604800
删：/d cache img-7d patterns=*.jpg ttl=604800   ← 同文本，只换前缀
```

导出 = 把内存状态 dump 为 w 命令序列。删除 = 把对应 w 改 d。

### A/B/C 三类命令

**A 类（直接命令，scope = 真实业务对象）**
```
/w domain   <host:port>     <origin>             写入域名
/w ssl      <域名/IP>       <动作或 ->          申请证书
/w sslfile  <域名/IP>       <选项或 ->          上传证书
```

**B 类（对象命令，scope = 对象名，无作用域）**
```
/w cache    <对象名>        <key=value 单行>     定义缓存对象
/w defense  <对象名>        <key=value 单行>     定义防御对象
/w redirect <对象名>        <key=value 单行>     定义重定向对象
/w header   <对象名>        <key=value 单行>     定义头部对象
```

**C 类（绑定命令）**
```
/w bind     <域名/IP>       <对象类型>:<对象名>
```

### 集群元数据查询（仅 v）
```
/v export   -               -                    导出全集群配置
/v status   -               -                    本节点状态
/v stats    -               -                    流量统计
/v nodes    [- | <peer-ip>] -                    peer 列表/详情
```

### 系统动作（独立动词，不入 w/d/v）
```
/sync                                              主动触发同步
/target  <peer-ip>                                 转移 bot 角色
/upgrade                                           触发集群升级
/menu /help                                        菜单 / 帮助
/confirm <ID>                                      二次确认
```

### 占位符 `-`

`-` 在所有字段统一为"兜底/默认/无限定"。具体含义：

- 域名字段 `-` = 任何 host（最低优先级）
- 源站字段 `-` = 本节点自己当源（默认页）
- v 命令的 scope `-` = 列出全部
- v 命令的 params `-` = 无细节限定

---

## 关键设计点

### 节点认亲
`secret = sha256(group_id + bot_token)`。任意 peer 都可接受认亲。

### 同步
单一 `config_version` 单调递增整数。snapshot 全量替换，不做 delta。落后节点从 RTT 最低的 peer 拉。主动推送 + 心跳兜底并行。

### 证书
- 来源：LE > 上传 > 自签（精度顺序）
- IP = 域名（零特异化）
- 申请节点：`candidates(D) = DNS A 记录包含本节点 IP 的节点集；responsible = candidates[hash(D) % len]`
- 选定后未过期不切换；过期立即按精度算法重选
- 自签 install.sh 阶段生成、100 年、不参与同步

### 升级
`runtime/` 全删，仅保留 `persistent/` 4 项：`identity.json` / `peers.json` / `certs/` / `snapshot.cmd`。

### 端口协议全局统一
每个端口在全集群有且仅有一个协议绑定。冲突 → 警告 + `/confirm`。

### 多绑定精度匹配
同域名绑定多个对象，按精度规则匹配；同精度冲突取"更严格"字段值（TTL 取最小、布尔取 OR）。匹配引擎委托 nginx，cdn-agent 不写运行时匹配。

---

## 项目布局速查

```
cmd/cdn-agent/main.go               入口
internal/
├── identity/      persistent/identity.json
├── peers/         persistent/peers.json
├── snapshot/      persistent/snapshot.cmd
├── db/            runtime/config.db
├── command/       ★肌肉层
│   ├── types.go         核心类型 + Handler 接口 + 解析器
│   ├── executor.go      批处理事务
│   └── handlers/        每种 type 一个文件
├── nginx/         runtime/nginx/* 生成
├── cert/          证书子系统
├── mesh/          ★血液层
├── bot/           Telegram（薄层）
├── cli/           cdn-agent exec（薄层）
└── version/       版本 + 嵌入源码
```

---

## 工程纪律

1. **包边界 = 物理边界**。一个内部包对应一个物理文件/目录/概念。
2. **handlers/ 一个 type 一个文件**。新增功能 = 新增一个 handler。
3. **构建必须经过 source-snapshot**。`make build` 自动 copy 源码到 `source/` 然后 embed 进二进制。`cdn-agent dump-source` 可恢复。**没有源码自包含的二进制不允许 release**。
4. **handler 不做 I/O**。Validate 纯逻辑、Write/Delete 通过事务、副作用通过 `Effects` 上报给 executor 统一处理。
5. **batch 内失败跳过继续**，但只有成功命令进版本号。

---

## 必查文档

- `V4-DESIGN.md` — 完整设计文档（约 1000 行，按需查阅）
- `types.go` — 命令系统所有核心类型定义（含详细注释）
- 本文件 — 总览（每次会话默认读）

---

**核心信条**：地基重要、配置即命令、命令即四段、节点平权、程序的边界是信息边界。
