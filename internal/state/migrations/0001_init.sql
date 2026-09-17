-- 0001_init.sql 初始 schema（docs/DATA_MODEL.md §2，rewrites 按计划 §4.8 用复合主键）
CREATE TABLE domains (
    id           INTEGER PRIMARY KEY,
    domain       TEXT NOT NULL UNIQUE,      -- 精确域名，punycode 小写
    zone         TEXT,                      -- 可注册域（zone 模式聚合时填）
    hit_count    INTEGER NOT NULL DEFAULT 0,-- 最近窗口内命中次数（每次运行重算）
    first_seen   TEXT NOT NULL,             -- 首次出现 RFC3339 UTC
    last_seen    TEXT NOT NULL,             -- 最后出现
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX idx_domains_last_seen ON domains(last_seen);

CREATE TABLE domain_probes (
    domain       TEXT PRIMARY KEY,
    cname_hit    INTEGER NOT NULL DEFAULT 0,-- 0/1
    cidr_hit     INTEGER NOT NULL DEFAULT 0,-- 0/1
    cf_ray_hit   INTEGER NOT NULL DEFAULT 0,-- 0/1
    server_hit   INTEGER NOT NULL DEFAULT 0,-- 0/1
    score        INTEGER NOT NULL DEFAULT 0,-- 加权总分
    confidence   TEXT NOT NULL,             -- confirmed | maybe | not_cf
    cname_chain  TEXT,                      -- 实际 CNAME 链，审计用
    resolved_ips TEXT,                      -- 探测时解析到的 A/AAAA（JSON 数组）
    error        TEXT,                      -- 探测失败原因（成功为 NULL）
    probed_at    TEXT NOT NULL
);

CREATE TABLE rewrites (
    domain       TEXT NOT NULL,             -- 与 domains.domain 对齐
    answer       TEXT NOT NULL,             -- 裸 IP（禁止写域名型 CNAME）
    ip_version   INTEGER NOT NULL,          -- 4 | 6
    state        TEXT NOT NULL,             -- active | pending | removed
    agh_present  INTEGER NOT NULL DEFAULT 0,-- 最近一次 list 中 AGH 是否已有该条
    first_synced TEXT,
    last_synced  TEXT,
    last_error   TEXT,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (domain, answer, ip_version)
);
CREATE INDEX idx_rewrites_state ON rewrites(state);

CREATE TABLE runs (
    id           INTEGER PRIMARY KEY,
    mode         TEXT NOT NULL,             -- dry | live
    started_at   TEXT NOT NULL,
    finished_at  TEXT,
    log_entries  INTEGER,                   -- 采集原始条目数
    candidates   INTEGER,                   -- 候选域名数
    confirmed    INTEGER,
    n_add        INTEGER NOT NULL DEFAULT 0,
    n_update     INTEGER NOT NULL DEFAULT 0,
    n_remove     INTEGER NOT NULL DEFAULT 0,
    n_failed     INTEGER NOT NULL DEFAULT 0,
    preferred_ip TEXT,                      -- 本次使用的优选 IP（多值时记录主 IP）
    note         TEXT                       -- 中止原因 / 汇总信息
);
