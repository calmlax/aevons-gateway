# Aevons Gateway Service

`aevons-gateway` 是 Aevons 自研 HTTP 网关服务，负责统一接入、认证校验、客户端资源控制、Consul 服务发现转发，以及多服务 Swagger 聚合展示。

## 目录

```text
aevons-gateway/
├── cmd/server
├── configs
├── internal
│   ├── auth
│   ├── clientauth
│   ├── config
│   ├── discovery
│   ├── model
│   ├── proxy
│   ├── router
│   └── swagger
├── swagger-ui
└── README.md
```

## 已实现能力

- 服务前缀路由匹配
- Consul 服务发现
- 基于 `auth-service` 的 Bearer Token 校验
- 客户端资源白名单控制，支持 `ALL`、精确路径、`/**` 前缀和 `ANY:/path/**`
- 企业级 Redis 网关限流
- 反向代理转发
- Swagger 源聚合与 Swagger UI 页面
- 网关请求上下文统一注入
- 健康检查、统一错误码和审计访问日志
- 默认已接入 `auth-service`、`sys-service`、`log-service`、`gen-service`、`job-service`

## 主要入口

- 服务地址：`http://127.0.0.1:11080`
- 健康检查：`GET /health`
- Swagger UI：`GET /swagger/`
- Swagger 源列表：`GET /api/v1/gateway/swagger/sources`
- Swagger JSON 代理：`GET /api/v1/gateway/swagger/:service/swagger.json`

## 配置

配置文件在 [configs/config.yaml](./configs/config.yaml)，主要分为：

- `server`
- `consul`
- `log`
- `cors`
- `xss`
- `gateway`
- `swagger`
- `services`
- `client_auth`

其中：

- `services` 定义服务路由前缀、发现方式、负载策略、公开接口白名单
- `client_auth.enabled=true` 时才启用 `oauth_client` 资源校验流程
- `gateway.discovery` 定义服务发现本地缓存刷新和失败兜底窗口
- `gateway.rate_limit` 定义限流能力开关、Redis key 前缀和 Consul KV 配置入口
- `swagger.allowed_ips` 控制 Swagger UI 和聚合接口访问来源
- `swagger.docs` 定义聚合展示的文档源，实际文档地址通过 `service_id + Consul 服务发现 + path` 解析

服务路由前缀说明：

- 当前使用单个 `prefix`
- 路由匹配走固定前缀判断，性能路径更简单直接
- 当前实现不支持 `/api/*/auth/**` 这类中间通配模式

服务发现缓存说明：

- 网关不会在每个请求上直接访问 Consul
- `gateway.discovery.refresh_seconds` 控制本地实例缓存刷新周期
- `gateway.discovery.stale_if_error_seconds` 控制 Consul 刷新失败时的短期旧实例兜底窗口
- 这样可以同时降低 Consul 压力，并减少 Consul 短暂抖动时的 503 放大

限流能力说明：

- 基于 Redis 固定窗口计数
- 支持按 `service / client / user / ip / path / method / global` 组合维度限流
- 支持默认规则和细粒度规则覆盖
- 支持 `fail_open`
- 支持从 Consul KV 加载复杂限流规则
- 命中后返回：
  - `429`
  - `X-RateLimit-Limit`
  - `X-RateLimit-Remaining`
  - `X-RateLimit-Reset`
  - `Retry-After`

本地配置建议只保留简版：

```yaml
gateway:
  rate_limit:
    enabled: true
    fail_open: true
    key_prefix: aevons:gateway:rate-limit:
    consul_kv_key: config/gateway/rate-limit
```

复杂规则建议放到 Consul KV，例如 key：

```text
config/gateway/rate-limit
```

value 内容可直接放 YAML：

```yaml
enabled: true
fail_open: true
key_prefix: aevons:gateway:rate-limit:
default:
  enabled: true
  key_by: [client, ip]
  limit: 600
  window_seconds: 60
rules:
  - name: auth-login
    enabled: true
    services: [auth-service]
    methods: [POST]
    paths: [/api/auth/v1/login]
    key_by: [ip, client]
    limit: 20
    window_seconds: 60
  - name: auth-passkey-login-begin
    enabled: true
    services: [auth-service]
    methods: [POST]
    paths: [/api/auth/v1/passkey/login/begin]
    key_by: [ip, client]
    limit: 15
    window_seconds: 60
  - name: auth-authorize
    enabled: true
    services: [auth-service]
    methods: [GET, POST]
    paths: [/api/auth/v1/authorize]
    key_by: [client, user]
    limit: 120
    window_seconds: 60
```

这样做的好处是：

- `config.yaml` 更简洁
- 限流规则可以独立治理
- 复杂规则不再堆在本地文件里
- 多实例网关可以统一读取同一份限流配置

如果 `consul_kv_key` 没配置，网关仍会回退使用本地 `config.yaml` 里的 `default/rules`。

限流命中返回值：

- 不需要配置 `status_code`
- 不需要配置 `message`
- 网关内部固定使用：
  - `status_code = 429`
  - `message = gateway.rate_limit_exceeded`

也就是说，大多数生产配置里：

- `limit`
- `window_seconds`
- `key_by`
- `services / methods / paths`

才是核心；返回码和消息已经由网关统一收口。

`key_by` 枚举说明：

- `global`
  整条规则共享一个计数桶。所有命中这条规则的请求，不再区分服务、用户、IP，全部累加到一起。适合做“全局总闸”。
- `service`
  按目标服务名分桶，例如 `auth-service`、`sys-service`。适合给不同下游服务独立配额。
- `client`
  按 OAuth `client_id` 分桶。适合限制某个第三方应用、前端应用或内部客户端的调用频率。未识别到客户端时记为 `anonymous`。
- `user`
  按当前登录用户分桶。适合做“单用户限流”，比如同一用户一分钟最多提交多少次。未登录时记为 `anonymous`。
- `ip`
  按客户端 IP 分桶。适合防刷、防爆破、防验证码滥用。
- `path`
  按请求路径分桶，例如 `/api/auth/v1/login`、`/api/auth/v1/authorize`。适合同一个服务下对不同接口做细分限制。
- `method`
  按 HTTP 方法分桶，例如 `GET`、`POST`。通常和 `service` 或 `path` 组合使用。

`key_by` 组合示例：

- `[ip]`
  表示每个 IP 单独计数，典型用于登录防爆破。
- `[client, ip]`
  表示同一个客户端下，再按 IP 细分。适合浏览器前端或第三方应用场景。
- `[client, user]`
  表示同一个客户端下，再按登录用户细分。适合 OAuth2 授权、个人操作类接口。
- `[service, method]`
  表示按目标服务 + 请求方法分桶，例如 `auth-service:POST` 和 `auth-service:GET` 分开统计。
- `[service, path, ip]`
  表示按目标服务 + 具体接口 + IP 分桶，粒度最细，但 Redis key 数量也会更多。

选择建议：

- 做全局保护：优先 `global`
- 做服务级保护：优先 `service`
- 做登录、防刷、防验证码：优先 `ip` 或 `[ip, client]`
- 做用户动作限制：优先 `user` 或 `[client, user]`
- 做某个具体接口的严格限制：优先 `path` 配合 `ip` / `client`

客户端资源规则缓存策略：

- 优先读 Redis
- Redis 未命中时回源数据库表 `sys_oauth_client`
- 回源结果写回 Redis，并按网关内部刷新周期自动过期
- 管理端修改 `sys_oauth_client.resources` 后，可调用 `sys-service` 的 `/api/sys/v1/oauth/client/refresh-cache` 立即更新网关缓存

`sys_oauth_client.resources` 规则说明：

- 推荐直接写路径规则，而不是服务名称
- 支持三种形式：
  - `ALL`
  - 精确路径：`/api/auth/v1/authorize`
  - 前缀路径：`/api/sys/v1/conf/*` 或 `/api/sys/v1/conf/**`

示例：

```text
ALL
```

```text
/api/auth/v1/authorize,/api/auth/v1/callback
```

```text
/api/sys/v1/conf/*,/api/sys/v1/menu/*
```

兼容说明：

- 旧的服务名称规则仍可继续识别
- 新配置建议统一收口到路径规则，粒度更细，也更适合后续治理

## 启动

```bash
go run ./cmd/server --config configs
```

如需指定环境配置：

```bash
APP_ENV=development go run ./cmd/server --config configs
```

## 校验

```bash
GOCACHE=/tmp/go-build-cache go test ./...
```
