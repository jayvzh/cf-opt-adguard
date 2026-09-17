# Release v0.1.0

## 功能亮点

- **全链路自动编排**：AGH querylog 只读采集 → 频次聚合 → 一级域归并（zone 内证据充分才下发通配，混入不确定域名自动回退逐条精确，防误伤）→ CF 探测确认 → 优选 IP → 增量计划 → AGH Rewrite 写入 → DNS 回查验证。
- **独立 CF 探测**：DNS CNAME 指向 + Cloudflare IP 段归属 + HTTP 响应头三信号打分，不轻信单一特征。
- **优选 IP 接入 CloudflareSpeedTest**：直接读取 CFST 的 `result.csv`，v4/v6 区分，多 IP 择优。
- **安全默认**：`run` 默认 dry-run 只打印计划，`--apply` 才写入；测速失败自动中止本轮，不复用旧结果。

## 前置要求

- 已运行 AdGuard Home（需管理员账号，REST API 可达）
- 已下载 [CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest) 并放到 cfst 目录（与 `cfst` 二进制同级）
- Linux amd64 / arm64，systemd（可选，无则回退 cron）
- 独立 DNS resolver（默认 223.5.5.5 / 119.29.29.29，勿指向本机 AGH，避免探测回路）

## 注意事项

- 首次使用建议先手动跑一次 dry-run 核对计划无误后再启用定时同步（`run` 默认即 dry-run，只打印计划不写入）：

  ```bash
  cd /opt/cfst && ./cfst -tl 200 -dn 20                    # 测速生成 result.csv
  ./cf-opt-adguard run -c config.yaml                      # 核对计划
  ./cf-opt-adguard run -c config.yaml --apply              # 确认无误后再写入
  ```

- 安装目录默认 `/opt/cf-opt-adguard`；`config.yaml` 含明文密码（600 权限），可改用 `${AGH_PASSWORD}` 环境变量形态。
- 卸载由 `install.sh` 的卸载菜单/子命令执行：停止定时任务并直接删除安装目录（含配置与数据），请提前备份 `config.yaml`。

## 免责声明

本工具修改家庭 / 内网 DNS 解析行为，误判可能影响个别站点访问，请在理解工作原理后使用，作者不承担因使用本工具产生的任何后果。
