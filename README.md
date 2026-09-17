# cf-opt-adguard

> 状态：**M1–M6 已实现（dry-run + `--apply` live 写入与 DNS 回查验证全链路可用）**。文档集已与代码核实对齐（2026-09-18）。

一个本地运行的 **AdGuard Home × Cloudflare 优选 IP 自动编排器**：从 AGH 查询日志挖掘高频域名，独立探测确认 Cloudflare CDN，把优选 IP 增量写为 AGH 的 DNS 重写规则，并持续维护（新站点自动加入、失效站点自动淘汰）。

## 它如何工作

```text
AGH 查询日志 → 采集 → 频次聚合 → CF 探测（独立 DNS + IP 段 + HTTP 头打分）
           → 优选 IP（CloudflareSpeedTest 结果文件（默认）/ 优选域名 / 静态列表）
           → 增量计划（add/update/remove）→ AGH Rewrite API → DNS 回查验证
```

- 外部独立 CLI，不改 AGH 核心、不做 AGH 插件；
- 默认按可注册域（zone）混合归并：zone 内证据充分才下发 `*.zone` 通配，混入不确定域名自动回退逐条精确，防误伤（D14）；默认 dry-run 预览，全程本地运行、不上传查询日志；

## 部署与使用

### 方式一：安装脚本

1. 把 tar.gz 上传到 目标机器下载解压后执行交互菜单：

   ```bash
   tar -xzf cf-opt-adguard-<版本>-linux-amd64.tar.gz && cd cf-opt-adguard-*
   sudo bash install.sh
   ```
   安装向导会依次询问：AGH 地址与凭据、统计窗口（24h/7d/30d/2w）、点击频次阈值、运行间隔（一行输入"天 小时"，如 `0 6` = 每 6 小时、`1 0` = 每天）、CFST 目录与测速命令（默认 `./cfst -tl 200 -dn 20`）、独立 resolver。

   定时任务：systemd 可用时注册 `cf-opt-adguard.service + .timer`（任意小时数精确）；否则回退 `/etc/cron.d/`（仅支持整点整除或整天步长，其他值就近取整并提示）。
 

2. 非交互安装示例：

```bash
sudo bash install.sh install \
    --agh-url http://192.168.1.2:3000 --agh-user admin --agh-pass 'xxx' \
    --window 24h --min-hits 20 --interval "0 6" \
    --cfst-dir /opt/cfst --cfst-cmd "./cfst -tl 200 -dn 20" \
    --install-dir /opt/cf-opt-adguard --skip-schedule   # 测试时可先不装定时器
```

### 方式二：手动运行

默认优选 IP 来源为 CloudflareSpeedTest 结果文件：先自行运行 CFST 测速，再把 `cf-opt-adguard` 二进制放入 CFST 目录（与 `cfst`、`result.csv` 同级）执行。


## 免责声明

本工具修改家庭 / 内网 DNS 解析行为，误判可能影响个别站点访问；`run` 默认即为 dry-run（只打印计划不写入），请核对计划无误后再加 `--apply` 执行，建议先在测试实例验证。
