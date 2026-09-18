# ARIS × Multica 自动化论文生产线 — 二开总体方案

- 状态：设计定稿（最终版，无 MVP 阶段划分）
- 基线：multica 仓库初始提交 `f5b8b33`
- 日期：2026-09-18
- 范围：基于 Multica 二开，结合 ARIS 研究/论文 skill 套件，构建"与主理人对话即可驱动的自动化论文生产线"

---

## 1. 背景与目标

ARIS skill 套件（idea-discovery、research-lit、experiment-plan、run-experiment、analyze-results、paper-write、auto-review-loop 等）覆盖了论文生产的全部智力环节，但缺乏宿主：没有统一的地方承接目标、拆解任务、调度专家、追踪进度、落盘产物。Multica 是"人与 agent 协作处理 issue"的平台，其执行内核（任务队列、daemon 认领、skill 物化、事件总线）已经产品级，缺的只是三块：

1. **确定性串接**：上一步完成自动放行下一步（现有 `issue_dependency` 表无任何触发逻辑）；
2. **主理人交互**：用户只面对一个聊天窗口，编排由一个 orchestrator agent 承担；
3. **ARIS 注入**：skill 批量注册与角色化 agent 花名册。

本方案给出这三块的最终设计。设计原则：最大化复用 Multica 现有机制（阶段屏障、mention 触发、autopilot、plugin hook），新增语义全部向后兼容（不使用依赖的 issue 行为完全不变）。

## 2. 总体架构

```
┌────────────────────────────────────────────────────────────┐
│ 交互层  用户 ↔ 主理人聊天窗口（现成 chat + aris-orchestrator） │
└──────────────┬─────────────────────────────────────────────┘
               │ multica issue create / metadata / runs（任务级 token）
┌──────────────▼─────────────────────────────────────────────┐
│ 编排层  流水线模板 → 实例化 → 父 issue + 阶段化子 issue        │
│         依赖门控（WP-1）：blocked_by 全部终态才放行入队        │
│         advance_mode = auto | orchestrator_review           │
└──────────────┬─────────────────────────────────────────────┘
┌──────────────▼─────────────────────────────────────────────┐
│ 执行层  专家 agents（scout/researcher/planner/experimenter/  │
│         analyst/writer/reviewer）绑定 ARIS skills，          │
│         daemon 认领执行，产物提交论文 git 仓库                 │
└──────────────┬─────────────────────────────────────────────┘
┌──────────────▼─────────────────────────────────────────────┐
│ 回传层  task.completed/failed 事件 → chat bridge（WP-3）→    │
│         主理人聊天窗口合并播报；人工决策门在聊天中完成          │
└────────────────────────────────────────────────────────────┘
```

两条铁律贯穿全部设计：

- **凡推进，必有服务端机制**：步骤放行、子任务唤醒、完成播报都不依赖提示词自觉；提示词只负责"判断与表达"。
- **凡人工，必有落点**：决策门要么是聊天中的一问一答，要么是 issue 的显式状态变更，不存在"等 agent 自己想起来问"。

## 3. 现状基线（可复用能力与关键代码位置）

以下为已核实的事实，二开直接建立在其上：

| 能力 | 位置 | 与本方案的关系 |
| --- | --- | --- |
| 运行触发判定 | `server/internal/service/issue_trigger.go:97`（`WillEnqueueRun`） | WP-1 的改造点：加入依赖判定 |
| 任务队列表 | `server/migrations/001_init.up.sql:127`（`agent_task_queue`，pending 去重唯一索引） | 所有触发的落点 |
| 依赖表（惰性） | `server/migrations/001_init.up.sql:89`（`issue_dependency`：blocks/blocked_by/related） | WP-1 激活对象 |
| 阶段屏障 | `server/migrations/123_issue_stage.up.sql`；`server/internal/handler/issue_child_done.go:70` | 最低未完成阶段全部终态时系统评论唤醒父 assignee；编排层"阶段收口"的现成机制 |
| 聊天会话 | `server/migrations/033_chat.up.sql`；`server/cmd/server/router.go:2257`（`/api/chat/sessions`） | 主理人窗口的宿主；会话绑定单 agent，`work_dir` 跨轮复用 |
| 聊天任务入队 | `server/internal/service/task.go:2297`（`SendDirectChatMessage`） | chat bridge 复用其入队路径 |
| 聊天回复落库 | `server/internal/service/task.go:3080`（`createAssistantChatMessage`） | agent 回合结束自动成为 `chat_message` |
| 班长协议范本 | `server/internal/handler/squad_briefing.go` | WP-3 主理人协议的蓝本（只协调不干活、派发即触发、汇报即唤醒、干完即停） |
| Autopilot | `server/migrations/042_autopilot.up.sql`；`server/internal/handler/autopilot.go:2365` | WP-5 实验 监控轮询的触发器 |
| Plugin hook | `server/pkg/plugincontract/manifest.go`；`server/internal/service/plugin_event_bridge.go:34` | 事件外发备用通道（与 chat bridge 二选一即可，默认用内置 bridge） |
| Skill 体系 | `server/migrations/008_structured_skills.up.sql`；`POST /api/skills`、`/api/skills/import`；daemon 侧物化 `server/internal/daemon/execenv/execenv.go:566` | WP-4 注入 ARIS skill 的通道 |
| 任务级令牌 | `server/internal/middleware/auth.go:110`（`mat_`，携带 runtime owner 身份） | agent 调 CLI/API 的凭证；chat bridge 权限规则的基础 |
| CLI | `server/cmd/multica/`（issue/autopilot/skill/squad/chat 等，`--output json`，退出码 0/2/3/4/5） | agent 与脚本的全部操作入口 |
| 事件总线 | `server/internal/events/bus.go`；`server/pkg/protocol/events.go` | chat bridge 的订阅源 |

两个必须绕开的坑：issue 处于 `backlog` 时任何派发都不触发（`issue_trigger.go` 中状态判定）；issue `triage_state='pending'` 时派生入队在队门被拦（`server/internal/service/task_triage_guard.go`）。本方案所有自动创建的 issue 一律 `status=todo`、`triage_state=accepted`。

## 4. 工作包总览

| 工作包 | 内容 | 语言/形态 | 依赖 |
| --- | --- | --- | --- |
| WP-1 | 依赖门控：激活 `issue_dependency`，终态放行 | Go（server） | 无 |
| WP-2 | 流水线模板、实例化、元数据约定 | Go（server）+ migrations | WP-1 |
| WP-3 | 主理人：编排 skill + chat bridge + `chat notify` | Skill + 少量 Go | WP-1、WP-2 |
| WP-4 | ARIS skill 同步脚本与 agent 花名册 | Python/Shell 脚本 | 无 |
| WP-5 | 实验长任务：submit-and-poll + autopilot 监控 | Skill 约定 + autopilot 配置 | WP-2 |
| WP-6 | 观测、治理与失败恢复 | 元数据约定 + 现有端点 | WP-2 |

## 5. WP-1 依赖门控（核心 Go 二开）

### 5.1 行为定义

- **放行规则**：issue 的 `blocked_by` 关系中，所有 blocker issue 的状态进入 `done` 或 `cancelled` 即视为依赖满足。
- **拦截点**：`WillEnqueueRun`（create/assign/status 三类触发）在入队前检查未满足的 `blocked_by`。未满足时不入队，行为与 backlog 停放一致：写操作本身成功、无独立 reason code、preview 不承诺运行（实现说明：backlog 停放同样静默，独立 reason code 反而破坏一致性；阻塞状态经 `GET /api/issues/{id}/dependencies` 的 `resolved` 字段暴露）。
- **放行点**：blocker issue 状态变更为 `done`/`cancelled` 的事务内，扫描以其为 blocker 的 `blocked_by` 关系；某 dependent 若因此全部满足且存在被拦截的触发意图，则走 `dispatchIssueRun` 入队，新增 `RunSource = dependency`。同一 dependent 由多个 blocker 完成并发放行时，靠 `agent_task_queue` 现有 pending 去重索引幂等。
- **失败语义**：blocker 对应**任务**失败不改变 issue 状态、不触发放行；chat bridge（WP-3）会向主理人播报失败，由主理人决策：重跑（`POST /api/issues/{id}/rerun`）或把 blocker issue 置 `cancelled`（视为裁剪范围，放行 dependent 并附系统评论说明"因 <blocker> 取消而带warning放行"）。`done` 与 `cancelled` 都放行，但 cancelled 放行必须在 dependent 上留痕。
- **环检测**：`PUT /api/issues/{id}/dependencies` 与批量接口在写入前做 DAG 校验（迭代染色），成环返回 400，拒绝落库。
- **兼容性**：无 `blocked_by` 的 issue 走原路径，行为逐字节不变。

### 5.2 实现要点

- 放行实现：`ListDependentIssues(depends_on_issue_id=$1, type='blocked_by')`（`server/pkg/db/queries/issue_dependency.sql`），再对每个 dependent 检查全部 blocker 终态；放行动作复用 child-done 屏障的"系统评论 + mention 入队"路径（`server/internal/handler/issue_dependency.go`），天然继承 pending 去重与就绪守卫，而非新增独立入队源。
- 现有测试基线：`squad_worker_comment_wakes_leader_test.go` 等同层测试给出 fixture 与断言风格；新增 `TestDependencyGate_*` 系列（放行、拦截、部分满足、cancelled 带警告放行、环拒绝、无依赖零回归）。
- `related` 关系维持惰性，不在本方案范围。

## 6. WP-2 流水线模板与实例化

### 6.1 概念模型

- **模板** = 有序阶段列表；每个阶段（stage）绑定：执行 agent、可选 ARIS skill 集合、提示词模板（含变量槽）、验收标准（acceptance criteria，主理人复核用）、`advance_mode`、并行组。
- **实例** = 1 个父 issue（assignee = 主理人 agent）+ 每阶段若干子 issue（assignee = 阶段 agent）。子 issue 写入 `issue.stage`（复用现有整数阶段列）与 `blocked_by` 链。
- **推进双通道**：
  - `advance_mode=auto`：阶段 N 全部子 issue 终态后，WP-1 依赖放行直接入队阶段 N+1 子任务；父屏障（`issue_child_done.go`）同时唤醒主理人做监督性播报，不阻塞推进。
  - `advance_mode=orchestrator_review`：阶段 N+1 子 issue 实例化为 `backlog`（天然不触发）；阶段 N 收口时父屏障唤醒主理人，主理人对照验收标准复核后手动 promote（状态变更加触发）。复核不通过则退回或在聊天中请示用户。

### 6.2 元数据约定（写入子 issue 与父 issue）

| 键 | 写在 | 含义 |
| --- | --- | --- |
| `pipeline_root` | 全部 | 父 issue id，全链路检索主键 |
| `pipeline_template` | 父 | 模板 id + 版本 |
| `pipeline_stage_name` | 子 | 阶段名 |
| `advance_mode` | 子 | `auto` / `orchestrator_review` |
| `orchestrator_session` | 父 | 主理人 chat session id，chat bridge 用 |
| `artifacts` | 子 | 阶段产物指针（仓库路径/分支、附件 id） |

检索统一走现成能力：`multica issue list --metadata pipeline_root=<id>`。

### 6.3 数据模型（迁移草案）

遵循仓库规则：不加外键；每个索引独立迁移文件且 `CREATE INDEX CONCURRENTLY`；条件对象后续 DDL 幂等。

```sql
-- xxxx_pipeline_template.up.sql
CREATE TABLE pipeline_template (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    description TEXT NOT NULL DEFAULT '',
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name, version)
);

CREATE TABLE pipeline_template_stage (
    id UUID PRIMARY KEY,
    template_id UUID NOT NULL,
    stage_order INTEGER NOT NULL,
    parallel_group INTEGER NOT NULL DEFAULT 0, -- 同组并行，同组内共享 issue.stage 值
    name TEXT NOT NULL,
    agent_id UUID NOT NULL,
    skill_ids UUID[] NOT NULL DEFAULT '{}',
    prompt_template TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    advance_mode TEXT NOT NULL DEFAULT 'auto'
        CHECK (advance_mode IN ('auto', 'orchestrator_review')),
    requires_human_gate BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

```sql
-- xxxx_pipeline_template_index.up.sql（独立文件）
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS pipeline_template_stage_order_idx
    ON pipeline_template_stage (template_id, stage_order);
```

`issue_dependency` 补一个查询用索引（同样独立文件、CONCURRENTLY）：

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_dependency_depends_on_idx
    ON issue_dependency (depends_on_issue_id, type);
```

### 6.4 API

| 端点 | 说明 |
| --- | --- |
| `GET/POST /api/pipeline-templates` | 列表 / 创建（含 stages 整体提交，服务端校验 stage_order 连续、agent 属于本 workspace） |
| `GET/PUT/DELETE /api/pipeline-templates/{id}` | 详情 / 整体更新（bump version，旧版本保留）/ 删除（有运行中实例则拒绝） |
| `POST /api/pipeline-templates/{id}/instantiate` | 实例化。入参：`{title, description, orchestrator_session_id?}`。实现说明（最终实现与设计初稿的偏差）：子 issue 全部以 backlog 创建（backlog 停放天然压制创建即触发），随后接 blocked_by 边，最后 auto 阶段经 WillEnqueueRun+dispatch 翻转为 todo——分阶段推进而非单事务，换来完全复用 IssueService.Create 的事件、编号与触发管线；失败时按创建逆序做应用层清理（表无外键） |
| `POST /api/issues/{id}/dependencies` | 维护 blocked_by（带环检测），供模板外手工补依赖 |
| `POST /api/pipeline-runs/{rootId}/advance` | 主理人 promote 专用：把 `orchestrator_review` 阶段的 backlog 子 issue 批量 promote（等价于逐个状态变更，走同一触发路径） |

兼容性规则（仓库 API 约束）：所有新端点响应在 `packages/core/api/schema.ts` 增加 zod schema 并走 `parseWithFallback`；配套 malformed-response 测试；可选字段给默认值。

## 7. WP-3 主理人聊天编排

### 7.1 aris-orchestrator skill（编排协议全文规格）

绑定到一个专职 agent（名字建议 `主理人`/`Aris`）。协议蓝本为 `squad_briefing.go`，改写为聊天版：

1. **受理**：用户陈述目标后，先确认四要素——研究方向与约束、目标发表层级、时间线、可用算力。要素不全先追问，不擅自开工。
2. **立项**：选择既有 `pipeline_template`（默认 `paper-default`）或现场定制计划；调用 `instantiate`；把父 issue 链接与阶段计划以摘要形式回贴聊天（每阶段一行：名称、执行 agent、advance_mode、验收标准）。
3. **派发**：不亲自执行任何阶段工作；阶段 agent 由模板指定。模板外的临时工作用 `multica issue create` + agent assignee（创建即触发）。
4. **监督**：被父屏障或 chat bridge 唤醒时，拉取 `issue list --metadata pipeline_root=<id>` 与 `issue run-messages` 汇总进度，对照验收标准做复核判定；`auto` 阶段只播报不拦截，`orchestrator_review` 阶段复核通过才 `advance`。
5. **请示**：阶段 `requires_human_gate=true`（默认：选题确认、大纲确认、终稿审）时，必须在聊天中给出结构化选项请用户拍板，用户回复（现有排队机制自动成为下一轮输入）后才继续。
6. **异常**：收到 task.failed 播报时，按"重跑一次 → 仍失败则汇报用户"处理；需要裁剪范围时明确说"将取消 <issue>，其下游将带警告放行"。
7. **收尾**：全部阶段终态后输出结项报告（产物清单、评审意见汇总、遗留项），父 issue 置 done，metadata `pipeline=completed`。
8. **纪律**：单次唤醒内完成"汇总→决策→派发/请示"即停（对应 squad 协议 no-action 语义）；不在聊天里长篇转述 issue 全文，只给结论与链接。

### 7.2 chat bridge（唯一的常驻新代码）

- 订阅：`cmd/server` 新增 listener（挂到现有 `listeners.go` 体系），订阅 `EventTaskCompleted` 与 `EventTaskFailed`。
- 路由：完成任务的 issue 带 `orchestrator_session` 元数据（沿 `pipeline_root` 找父 issue 读取）才处理；否则零开销跳过。
- 合并：同一 session 30 秒窗口内多条事件合并为一条结构化播报（issue refs、状态、失败原因、建议动作），避免刷屏；窗口参数进 config。
- 投递：调用新方法 `ChatService.PostSystemTurn(sessionID, payload)`——插入 `actor_type=system` 的 chat message 并复用 `SendDirectChatMessage` 的入队路径唤醒主理人；WS 侧复用 `chat:message` 事件，前端无需改动即可渲染（系统消息样式为渐进增强）。
- 失败不丢：bridge 投递失败进入现有 webhook delivery 风格的重试队列（复用 `server/internal/handler/webhook_delivery_worker.go` 的模式），最终失败打点日志。

### 7.3 `multica chat notify`（替代隐式权限漏洞）

新增 CLI 子命令与端点 `POST /api/chat/sessions/{id}/notify`：

- 调用者须为任务级 token（`mat_`）且其 runtime owner 等于会话 creator（把现状的"隐式巧合"变成显式规则，`loadChatSessionForUser` 增加对应判定分支，creator 本人与该规则并行有效）。
- 语义：向会话投递一条系统侧消息并入队主理人回合；专家 agent 汇报、autopilot 通知都走这里。
- 权限测试覆盖：creator 本人 ✓、同 owner 任务 token ✓、他人 token ✗ 403、无 token ✗ 401。

### 7.4 会话与运行时

- 主理人 agent 的 provider 会话续接受限：`PriorSessionID` 仅同 runtime 生效（`handler/daemon.go:3131`）。主理人绑定固定 runtime（bootstrap 脚本落配置），跨轮记忆不依赖 skill 上下文重建。
- 主理人每轮的开销控制：协议第 8 条 + bridge 合并播报 + 汇总用 metadata 查询而非逐 issue 拉全文。

## 8. WP-4 ARIS 技能接入与 Agent 花名册

### 8.1 skill 同步

- 源：`aris-skills/` git 仓库（从 `~/.agents/skills` 迁移固化，消灭"本机目录"这个不可复制状态）。
- 同步脚本 `scripts/aris/sync_skills.py`：遍历仓库内 skill 目录（SKILL.md + 附属文件），以 skill name 为幂等键调 `POST /api/skills`（存在则更新 content 与 files），输出 diff 报告；PAT 从环境变量 `MULTICA_PAT` 读取，绝不落盘。
- 绑定脚本 `scripts/aris/bind_agents.py`：按花名册调 `PUT /api/agents/{id}/skills`。同步与绑定分离：skill 库全局共享，agent 绑定才决定可见性。

### 8.2 花名册（bootstrap 基线）

| Agent | ARIS skills | 流水线阶段 |
| --- | --- | --- |
| Aris（主理人） | aris-orchestrator | 全程编排 |
| Scout | idea-discovery、novelty-check | 选题 |
| Researcher | research-lit、arxiv、comm-lit-review | 文献综述 |
| Planner | experiment-plan、ablation-planner | 实验设计 |
| Experimenter | run-experiment、monitor-experiment | 跑实验 |
| Analyst | analyze-results、result-to-claim | 分析与结论 |
| Writer | paper-write、paper-figure、paper-compile、humanizer-zh | 成文与图表 |
| Reviewer | auto-review-loop、rebuttal | 内审与修订 |

`scripts/aris/bootstrap.sh` 一键完成：创建 agents（最小 instructions：角色一句话 + 升级路径）、同步 skills、绑定、创建 `paper-default` 模板、建主理人聊天会话。幂等可重跑。

### 8.3 默认模板 `paper-default`

| # | 阶段 | agent | advance | 人工门 |
| --- | --- | --- | --- | --- |
| 0 | 选题 | Scout | orchestrator_review | ✓ 选题确认 |
| 1 | 文献综述 | Researcher | orchestrator_review | ✓ 大纲确认 |
| 2 | 实验设计 | Planner | auto | — |
| 3 | 跑实验 | Experimenter | auto（受 WP-5 监控节奏约束） | — |
| 4 | 分析 | Analyst | auto | — |
| 5 | 成文 | Writer | auto | — |
| 6 | 内审修订 | Reviewer | auto（可循环：Review→Writer 回路走 blocked_by 重开子 issue） | — |
| 7 | 终稿 | Writer | orchestrator_review | ✓ 终稿审 |

## 9. WP-5 实验长任务与外部集群

**决策：submit-and-poll，训练不占用 daemon 任务生命周期。**

- Experimenter 的 run-experiment skill 改造为"提交即返回"：向集群（slurm/云）提交作业，`cluster_job=<id>`、`cluster_status=submitted` 写入 issue metadata，当前任务正常结束，issue 留在 `in_progress`。
- 建配套 autopilot（cron 15 分钟，`execution_mode=create_issue` 改为 **run_only**：对实验 issue 评论 `@Experimenter 轮询 cluster_job`）：Experimenter 每次被唤醒查集群状态——运行中则更新 metadata 后即停；完成则拉取产物入库、issue 置 done（触发放行）；失败则按 WP-3 第 6 条处理。
- 收敛保护：autopilot 配 concurrency `skip`；metadata `cluster_status=terminal` 后主理人删除/停用该监控项（结项清理清单的一步）。
- 理由：daemon 任务挂几小时占资源且易碎；submit-and-poll 让"长"变成 metadata 状态而不是进程存活，天然可恢复。

## 10. WP-6 观测、治理与失败恢复

- **进度面板**：主理人聊天即面板； issue 侧用保存的过滤器 `metadata pipeline_root=<id>`；不做新 UI（最终版不引入前端二开）。
- **审计**：全部复用现有 task runs / autopilot runs / 事件流；bridge 播报含 issue ref 可回溯。
- **恢复**：单任务失败 → `rerun`；阶段卡死（dependencies 长期未满足）→ 主理人监督播报中高亮；整个流水线重跑 → 以同模板重新 instantiate 新父 issue，旧链 metadata `pipeline=archived`。
- **清理**：结项时主理人执行固定清单（停监控 autopilot、归档 metadata、产物附件校验）。

## 11. 关键流程时序

**立项**：用户聊天陈述目标 → 主理人补齐四要素 → `instantiate`（单事务：父+子 issue、stage、blocked_by、metadata）→ 第 0 阶段入队（或因 review 模式停 backlog）→ 主理人回贴计划摘要。

**自动推进（auto）**：阶段 N 末个任务终态 → WP-1 同事务放行阶段 N+1 子任务 + 父屏障系统评论唤醒主理人 → 主理人拉进度、聊天播报（bridge 已合并推送则本轮只做监督记录）。

**人工门（orchestrator_review）**：阶段收口 → 屏障唤醒主理人 → 复核验收标准 → 聊天请示用户（结构化选项）→ 用户回复 → `advance` → 下一阶段入队。

**失败恢复**：任务失败 → chat bridge 播报（含 rerun 建议）→ 主理人先 `rerun` → 仍失败则请示用户 → 决策：重试/换路径/取消 blocker 带警告放行 → 全程 issue 留痕。

## 12. 权限与安全

- 所有自动化入口用 PAT；agent 运行内操作走任务级 `mat_` token，归属仍记到 originator 用户（现成机制）。
- chat session 仍 creator-only；agent 侧新增的 notify 通道以"runtime owner == creator"为界，单人自托管场景天然满足，多人场景不越界。
- 私有 agent 可见性与 `canInvokeAgent` 不放宽；花名册 agents 全部 workspace 级可见。
- 凭证（集群私钥、API key）只经环境变量进入 Experimenter 任务（`custom_env`），不写入模板、metadata、聊天。

## 13. 测试与验收

| 层 | 内容 |
| --- | --- |
| Go 单测（`server/internal/service`、`handler`，用 `testutil` fixture） | WP-1 全矩阵（放行/拦截/部分满足/cancelled 警告放行/环拒绝/无依赖零回归）；`advance` 端点；notify 权限矩阵；bridge 合并窗口与失败重试 |
| malformed-response 测试 | 新端点响应 schema（zod + `parseWithFallback`） |
| e2e（`e2e/`，TestApiClient） | instantiate → 首阶段触发 → 模拟完成 → 依赖放行 → review 门 promote → 结项 metadata |
| Skill 验收（不进默认测试） | bootstrap 幂等重跑；paper-default 干跑一条玩具流水线（fake executable，遵守"默认测试不得解析真实 agent CLI"规则） |

**整线验收标准**：向主理人发一条目标消息后，到收到结项报告为止，用户在聊天外的操作为 0；三次人工门均在聊天内完成；期间任一阶段失败均由主理人主动播报并给出恢复动作。

## 14. 实施顺序与规模

| 序 | 工作包 | 预估规模 |
| --- | --- | --- |
| 1 | WP-1 依赖门控（含迁移、测试） | Go 中等，~1-2 周 |
| 2 | WP-2 模板与实例化 | Go + API，~1-2 周 |
| 3 | WP-4 skill 同步与 bootstrap（可与 1、2 并行） | 脚本，~2-3 天 |
| 4 | WP-3 chat bridge + notify + 主理人 skill | Go 小 + skill 文档为主，~1 周 |
| 5 | WP-5 实验监控改造 | skill + autopilot 配置，~2-3 天 |
| 6 | WP-6 收尾联调 + e2e | ~3-5 天 |

全部完成后的常态使用形态：一个聊天窗口、一个论文 git 仓库、若干专家 agent；用户只与主理人说话。

## 15. 风险与开放问题

1. **复核门质量依赖主理人判定**：orchestrator_review 阶段的验收复核是模型判断，可能误放行。缓解：验收标准在模板中要求可机检项（文件存在、图表数量、编译通过），主理人只审语义项。
2. **bridge 合并窗口调参**：过短刷屏、过长迟滞；上线后按实际节奏调，先 30s。
3. **provider 会话续接的 runtime 绑定**：主理人换 runtime 丢聊天记忆（ Multica 现状约束）。缓解：bootstrap 固定绑定；skill 要求关键结论回写父 issue（issue 才是持久真相源，聊天只是界面）。
4. **Review→Writer 回环**：修订轮次多时子 issue 数量膨胀。缓解：回环复用同一对子 issue（重开而非新建），轮次计数入 metadata。
5. **上游同步**：本方案触碰 `issue_trigger.go`、`dispatch/reason.go` 等核心文件，未来 rebase 上游需关注 `WillEnqueueRun` 与 chat 模块的变更；迁移文件遵循 CONCURRENTLY 规则可降低冲突面。
