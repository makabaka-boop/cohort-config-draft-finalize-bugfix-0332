# configrel — 路由/限额配置按版本发布服务

一个内容站点的 **routes** 与 **limits** 两组配置必须作为**同一个不可变包**发布，
任何请求都不可能读到“新路由 + 旧限额”。本仓库用 Go API + PostgreSQL 实现：

- **草稿（draft）**：可编辑的 routes/limits 工作副本；提交前校验组内结构与
  **跨组引用**（`route.limit_id` 必须在同一份 limits 中存在）。
- **包（package）**：草稿定稿后成为不可变快照（数据库触发器拒绝
  UPDATE/DELETE）。包按 `sha256(canonical(routes) + canonical(limits))`
  内容寻址，两组内容一起决定包号，缺一不可。
- **发布代次（generation）**：每次发布、比例调整、回滚都追加一条新记录并
  推进单调递增的代次；提交必须携带 `expected_generation`，基于单行指针
  `SELECT … FOR UPDATE` 做乐观并发控制——两个 API 实例同时提交只有一个成功，
  另一个得到 `409` 与当前代次。
- **试用比例 0–100%**：按客户端 ID 的 **FNV-1a 稳定哈希**分桶（0–99），
  分组结果不依赖任何进程内状态，因此所有实例、重启前后完全一致。
- **最低客户端版本**：发布声明 `min_client_version`（数字点分版本号）。
  解析配置时从最新代次向前找：版本不满足、或未进入试用桶的代次一律跳过，
  回退到**最近的兼容包**。响应同时给出实际 `package_id`、命中的
  `generation` 与当前 `head_generation`、`served_head`。
- **完整快照**：查询、发布记录、回滚都通过 JOIN 取出对应包的完整
  routes/limits；回滚只能指向历史上发布过的包。

## 目录结构

```
cmd/api/        HTTP 服务入口
cmd/verify/     一次性端到端验收程序（compose 的 verify 服务）
internal/configmodel/  领域模型：校验、校验和、版本比较、稳定哈希、解析
internal/store/        PostgreSQL 存储（schema.sql 内嵌）
```

## 快速开始（固定验收）

```bash
docker compose config --quiet
docker compose build
docker compose run --rm verify
```

`verify` 会等待 API 健康后依次验证：草稿跨组校验、定稿不可变、校验和同时
绑定两组、两实例并发发布仅一个成功、比例边界（0%/100%/-1/101）、旧客户端
回退、跨实例与重启后的稳定分组、调整/回滚产生新代次且指向完整快照、过期
代次被拒绝。全部通过退出码为 0。

只启动服务：

```bash
docker compose up -d db api api2
curl -s localhost:8080/healthz
```

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/drafts` | 建草稿 |
| GET | `/v1/drafts/{id}` | 查草稿 |
| PUT | `/v1/drafts/{id}/content` | 写入/替换 routes、limits（立即校验） |
| POST | `/v1/drafts/{id}/finalize` | 校验跨组引用并冻结为不可变包 |
| GET | `/v1/packages/{id}` | 取完整包（routes+limits） |
| POST | `/v1/releases` | 发布：`package_id`、`rollout_pct`(0-100)、`min_client_version`、`expected_generation` |
| POST | `/v1/releases/adjust` | 调比例/最低版本（产生新代次，沿用当前包） |
| POST | `/v1/releases/rollback` | 回滚到历史包（产生新代次） |
| GET | `/v1/releases` | 发布记录（每条内嵌完整包），新到旧 |
| GET | `/v1/releases/current` | 当前头部代次及完整快照 |
| GET | `/v1/releases/{generation}` | 取指定代次记录及完整快照 |
| GET | `/v1/resolve?client_id=&client_version=` | 客户端取配置 |

### 典型流程

```bash
# 1) 草稿
curl -s -XPOST localhost:8080/v1/drafts
curl -s -XPUT localhost:8080/v1/drafts/1/content \
  -H 'content-type: application/json' -d '{
    "routes": [{"id":"r1","path":"/a","limit_id":"l1"}],
    "limits": [{"id":"l1","name":"basic","rpm":100}]}'
curl -s -XPOST localhost:8080/v1/drafts/1/finalize   # -> package id

# 2) 以 30% 试用发布，要求客户端 >= 1.2.0（首次 expected_generation=0）
curl -s -XPOST localhost:8080/v1/releases \
  -H 'content-type: application/json' \
  -d '{"package_id":1,"rollout_pct":30,"min_client_version":"1.2.0","expected_generation":0}'

# 3) 客户端解析
curl -s 'localhost:8080/v1/resolve?client_id=client-42&client_version=1.2.0'
# -> {package_id, generation, head_generation, served_head, routes, limits, ...}

# 4) 调整比例（新代次）
curl -s -XPOST localhost:8080/v1/releases/adjust \
  -H 'content-type: application/json' -d '{"rollout_pct":100,"expected_generation":1}'

# 5) 回滚到历史包（新代次）
curl -s -XPOST localhost:8080/v1/releases/rollback \
  -H 'content-type: application/json' \
  -d '{"package_id":1,"expected_generation":2}'
```

并发提交时失败方返回：

```json
HTTP 409
{"error":"conflict: expected generation 1 but head is 2","current_generation":2}
```

## 本地开发

```bash
go test ./...                            # 纯单元测试（哈希、版本、解析、校验和）
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # PG 集成测试
```

集成测试为每个进程使用独立 schema，包含：并发提交只有一个赢家、包不可变
触发器、内容寻址复用、调整/回滚代次、0–100 约束等。

## 设计要点

- 定稿在单个事务内：`SELECT … FOR UPDATE` 锁住草稿行 → 按校验和查/插包
  （唯一约束兜底并发，不使用 `ON CONFLICT DO UPDATE` 以遵守不可变触发器）
  → 草稿置为 finalized 并记录包号。
- 发布在单个事务内锁住 `release_state` 单行指针，先比对
  `expected_generation` 再插入代次并推进指针；读历史用单条读一致性 JOIN，
  代次与包不可能错配。
- 分桶：`FNV1a(client_id) % 100 < rollout_pct`；0% 永不命中，100% 全部
  命中。算法在所有实例的共享代码中，无内存状态 → 重启稳定。
