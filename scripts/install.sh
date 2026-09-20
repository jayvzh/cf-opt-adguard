#!/usr/bin/env bash
# cf-opt-adguard 多菜单安装管理脚本
# 功能：安装二进制 + 交互配置 + CFST 调速包装 + systemd/cron 定时；支持本地包或 GitHub 拉取。
# 样式参考：references/install_nodeget.sh
set -e

APP_NAME="cf-opt-adguard"
SERVICE_NAME="cf-opt-adguard"
DEFAULT_INSTALL_DIR="/opt/cf-opt-adguard"
# 管理菜单安装位置：脚本自身副本存到安装目录，软链到 PATH 形成快捷命令。
MANAGE_BIN="/usr/local/bin/${APP_NAME}"
# 安装位置指针（root 安装时写入，供快捷命令在自定义安装目录时恢复路径）。
POINTER_FILE="/etc/${APP_NAME}.conf"
# GitHub releases 拉取基址（构建时 build-release.sh 会重新注入；此处为兜底默认）。
DEFAULT_RELEASE_BASE_URL="https://github.com/jayvzh/cf-opt-adguard/releases/latest/download"
# 脚本自身的规范获取地址（用于提示与 curl|bash 场景）。
SCRIPT_URL="https://github.com/jayvzh/cf-opt-adguard/raw/refs/heads/main/scripts/install.sh"
# CloudflareSpeedTest 官方最新发布（资产名 cfst_linux_<arch>.tar.gz，内含 cfst 二进制）。
CFST_RELEASE_BASE="https://github.com/XIU2/CloudflareSpeedTest/releases/latest/download"

action=""
arch=""
install_dir=""
install_dir_explicit=false   # 是否经 --install-dir 显式指定（指定后安装向导不再询问目录）
arg_url=""          # --url 覆盖拉取基址
skip_schedule=false
with_cfst=false     # --with-cfst：安装时自动拉取 cfst 依赖
MANAGE_SHORTCUT_OK=false  # 本次安装是否成功创建快捷命令软链

# 交互答案（parse_args 可预置，do_install 中逐项补默认）
agh_url=""; agh_user=""; agh_pass=""
window=""; min_hits=""
interval_days=""; interval_hours=""
cfst_dir=""; cfst_cmd=""; resolvers=""

_red()   { echo -e "\033[0;31m$1\033[0m"; }
_yellow(){ echo -e "\033[0;33m$1\033[0m"; }
_blue()  { echo -e "\033[0;36m$1\033[0m"; }
_green() { echo -e "\033[0;32m$1\033[0m"; }
_white() { echo -e "\033[0;37m$1\033[0m"; }
# 加粗色（标题/强调用，样式参考 references/install_nodeget.sh）
_red_bold()   { echo -e "\033[1;31m$1\033[0m"; }
_yellow_bold(){ echo -e "\033[1;33m$1\033[0m"; }
_blue_bold()  { echo -e "\033[1;36m$1\033[0m"; }
_green_bold() { echo -e "\033[1;32m$1\033[0m"; }
_white_bold() { echo -e "\033[1;37m$1\033[0m"; }

########################################
# 环境检测
########################################
detect_env() {
    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) _red "不支持的架构: $(uname -m)（仅支持 amd64/arm64）"; exit 1 ;;
    esac
    SYSTEMD=0
    [ -d /run/systemd/system ] && SYSTEMD=1
}

is_root() { [ "$(id -u)" = "0" ]; }

require_cmds() {
    local miss="" c
    for c in curl tar; do command -v "$c" >/dev/null 2>&1 || miss="$miss $c"; done
    if [ -n "$miss" ]; then
        _red "缺少必要命令:$miss，请先安装后再运行本脚本。"; exit 1
    fi
}

release_base_url() {
    if [ -n "$arg_url" ]; then echo "$arg_url"; return; fi
    echo "$DEFAULT_RELEASE_BASE_URL"
}

########################################
# 获取二进制：本地二进制 > 本地 tar.gz > 远程拉取
########################################
fetch_binary() {
    local dest_dir="$1" src tarball script_dir
    script_dir=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || script_dir=""

    # 管道执行（bash <(curl ...)）时 $0 类似 /dev/fd/63，直接跳过本地探测走远程。
    if [ -n "$script_dir" ] && [ -d "$script_dir" ] && [[ "$script_dir" != /dev/fd/* ]]; then
        if [ -x "$script_dir/cf-opt-adguard" ]; then
            src="$script_dir/cf-opt-adguard"
            _blue "使用同目录二进制: $src"
            cp "$src" "$dest_dir/cf-opt-adguard"
            return 0
        fi

        tarball=$(ls "$script_dir"/cf-opt-adguard-*-linux-*.tar.gz 2>/dev/null | head -n1 || true)
        if [ -n "$tarball" ]; then
            _blue "使用同目录发布包: $tarball"
            tar -xzf "$tarball" -C "$dest_dir" --strip-components=1 cf-opt-adguard*-linux-*/cf-opt-adguard
            return 0
        fi
    fi

    local base; base=$(release_base_url)
    if [ -z "$base" ]; then
        _red "未找到本地二进制/发布包，且未配置拉取地址。"
        _yellow "请把发布 tar.gz 或解压后的二进制与本脚本放同一目录，或用 --url 指定拉取基址。"
        exit 1
    fi
    # 跟随 /releases/latest 重定向取真实 tag，拼出确定资产名。
    local repo_root latest tag
    repo_root="${base%/releases/*}"
    latest=$(curl -fsS -o /dev/null -w '%{redirect_url}' "$repo_root/releases/latest" || true)
    tag=${latest##*/}
    if [ -z "$tag" ]; then
        _red "获取最新版本号失败（GitHub 不可达或尚无 Release）: $repo_root/releases/latest"
        exit 1
    fi
    tarball="cf-opt-adguard-${tag}-linux-${arch}.tar.gz"
    _blue "拉取发布包: $base/$tarball"
    curl -fL "$base/$tarball" -o "$dest_dir/$tarball"
    tar -xzf "$dest_dir/$tarball" -C "$dest_dir" --strip-components=1 \
        "cf-opt-adguard-${tag}-linux-${arch}/cf-opt-adguard"
    rm -f "$dest_dir/$tarball"
}

########################################
# CloudflareSpeedTest 依赖：从官方 Release 拉取最新版解包到 cfst 目录
########################################
fetch_cfst() {
    local dir="$1" tmp url
    mkdir -p "$dir"
    url="$CFST_RELEASE_BASE/cfst_linux_${arch}.tar.gz"
    _blue "==> 拉取 CloudflareSpeedTest 最新版（linux/${arch}）"
    tmp=$(mktemp -d)
    if ! curl -fL --retry 2 "$url" -o "$tmp/cfst.tar.gz"; then
        _red "cfst 下载失败: $url"; rm -rf "$tmp"; return 1
    fi
    # 包内顶层目录为 cfst_linux_<arch>/，strip 一层落到 cfst 目录（含 cfst/ip.txt/ipv6.txt）。
    tar -xzf "$tmp/cfst.tar.gz" -C "$dir" --strip-components=1
    rm -rf "$tmp"
    chmod +x "$dir/cfst"
    if [ ! -x "$dir/cfst" ]; then
        _red "cfst 解包后未找到可执行文件: $dir/cfst"; return 1
    fi
    _green "[ok] cfst 已就绪: $dir/cfst"
}

# 安装向导收尾阶段：cfst 缺失时按模式自动/询问拉取。
ensure_cfst() {
    [ -x "$cfst_dir/cfst" ] && return 0
    if [ "$with_cfst" = true ]; then
        fetch_cfst "$cfst_dir"
    elif [ -t 0 ]; then
        if ask_yesno "未检测到 $cfst_dir/cfst，是否自动下载 CloudflareSpeedTest 最新版" y; then
            fetch_cfst "$cfst_dir"
        else
            _yellow "已跳过；稍后可用管理菜单「6. 更新 CloudflareSpeedTest」补装。"
        fi
    else
        _yellow "未检测到 $cfst_dir/cfst；非交互模式可加 --with-cfst 自动下载。"
    fi
}

########################################
# 配置读写（settings.env 保存全部答案，供重配置/重装复用）
########################################
settings_file() { echo "$install_dir/settings.env"; }

# 未显式 --install-dir 时恢复上次安装路径：先读 /etc 指针文件，再回退默认目录 settings.env。
load_install_dir() {
    [ -n "$install_dir" ] && return 0
    local line id f
    if [ -r "$POINTER_FILE" ]; then
        line=$(grep -m1 '^INSTALL_DIR=' "$POINTER_FILE") || true
        id=${line#INSTALL_DIR=}; id=${id%\'}; id=${id#\'}
        [ -n "$id" ] && { install_dir=$id; return 0; }
    fi
    f="$DEFAULT_INSTALL_DIR/settings.env"
    [ -f "$f" ] || return 0
    line=$(grep -m1 '^INSTALL_DIR=' "$f") || return 0
    id=${line#INSTALL_DIR=}; id=${id%\'}; id=${id#\'}
    [ -n "$id" ] && install_dir=$id
}

load_settings() {
    local f; f=$(settings_file)
    [ -f "$f" ] && . "$f" || true
}

# 单引号包裹（shell 安全），YAML 用同函数 + yaml_quote
sh_quote() { local s=$1; s=${s//\'/\'\\\'\'}; printf "'%s'" "$s"; }
# YAML 单引号转义：' → ''
yaml_quote() { local s=$1; s=${s//\'/\'\'}; printf "'%s'" "$s"; }

ask_defaults() {
    # 已有 settings.env 的旧值做默认，回车沿用。
    agh_url=${agh_url:-${AGH_URL:-}}
    agh_user=${agh_user:-${AGH_USER:-admin}}
    agh_pass=${agh_pass:-${AGH_PASS:-}}
    window=${window:-${WINDOW:-7d}}
    min_hits=${min_hits:-${MIN_HITS:-10}}
    interval_days=${interval_days:-${INTERVAL_DAYS:-0}}
    interval_hours=${interval_hours:-${INTERVAL_HOURS:-12}}
    cfst_dir=${cfst_dir:-${CFST_DIR:-$install_dir/cfst}}
    cfst_cmd=${cfst_cmd:-${CFST_CMD:-./cfst -tl 200 -dn 20}}
    resolvers=${resolvers:-${RESOLVERS:-223.5.5.5,119.29.29.29}}
}

# ask_str <变量名> <提示> <默认值>：统一"回车采用默认值"输入，提示格式「提示 [默认值]: 」。
# 结果通过 printf -v 写回调用者指定变量；EOF / 直接回车均取默认值。
ask_str() {
    local __var=$1 __prompt=$2 __def=$3 __val
    read -rp "$__prompt [$__def]: " __val || true
    printf -v "$__var" '%s' "${__val:-$__def}"
}

# ask_install_dir 安装第 0 步：确认安装目录，回车使用默认路径（或已安装的旧路径）；
# 系统目录非 root / 空输入原地报错重输，避免走到 mkdir 才失败。
ask_install_dir() {
    local __def=${install_dir:-$DEFAULT_INSTALL_DIR} __in
    echo
    _blue_bold "── 0/3 安装目录 ────────────────────────────"
    while true; do
        read -rp "安装目录（回车使用 $__def）: " __in || { _red "输入流已关闭"; exit 1; }
        install_dir=${__in:-$__def}
        # read 不做 tilde 展开，手动处理 ~；相对路径转绝对（指针文件 / settings 依赖绝对路径）。
        install_dir=${install_dir/#\~\//$HOME/}
        install_dir=${install_dir/#\~/$HOME}
        case "$install_dir" in /*) ;; *) install_dir="$PWD/$install_dir" ;; esac
        if [ "${#install_dir}" -gt 1 ]; then install_dir=${install_dir%/}; fi
        if [ -z "$install_dir" ] || [ "$install_dir" = "/" ]; then
            _red "安装目录不能为空或为根目录，请重新输入"
            continue
        fi
        case "$install_dir" in
            /opt/*|/etc/*|/usr/*|/var/*)
                is_root || { _red "写入 $install_dir 需要 root；请重新输入用户目录（如 $HOME/$APP_NAME），或用 sudo 运行。"; continue; } ;;
        esac
        break
    done
}

# ask_interval 运行间隔交互：读整行解析"天 小时"两个非负整数，回车沿用当前值；
# 非法格式（只输一个值 / 非数字 / 总间隔 <1 小时）原地报错重输。
ask_interval() {
    local __line __d __h
    local -a __toks
    while true; do
        read -rp "运行间隔（天 小时，如 0 6=每6小时、1 0=每天；回车沿用 $interval_days $interval_hours）: " __line || return 1
        [ -z "$__line" ] && return 0
        # read -a 只分词不做 glob 展开，避免输入 * 等字符被展开成文件名。
        read -ra __toks <<< "$__line"
        __d=${__toks[0]:-}; __h=${__toks[1]:-}
        if [ "${#__toks[@]}" -eq 2 ] \
            && case "$__d" in ''|*[!0-9]*) false ;; *) true ;; esac \
            && case "$__h" in ''|*[!0-9]*) false ;; *) true ;; esac \
            && [ $((10#$__d * 24 + 10#$__h)) -ge 1 ]; then
            interval_days=$__d; interval_hours=$__h
            return 0
        fi
        _red "间隔格式非法：请输入两个以空格分隔的非负整数（天 小时），且总间隔至少 1 小时；请重新输入"
    done
}

# ask_yesno <提示> <y|n>：统一 (Y/n) 确认；回车取默认值（第二参数 y / n），
# 仅接受 y/yes/n/no（不区分大小写），非法输入报错重问；EOF 视为取消（返回 1）。
ask_yesno() {
    local __prompt=$1 __def=$2 __r __hint="(Y/n)"
    [ "$__def" = "n" ] && __hint="(y/N)"
    while true; do
        read -rp "$__prompt $__hint: " __r || return 1
        case "${__r:-$__def}" in
            y|Y|yes|Yes|YES) return 0 ;;
            n|N|no|No|NO) return 1 ;;
            *) _red "请输入 y 或 n" ;;
        esac
    done
}

# agh_precheck 同步前置预检：settings 凭据齐全时先验证 AGH 可达且凭据有效，
# 失败返回 1（run-once 场景避免 CFST 测速数分钟后才发现 AGH 故障，白跑一轮）。
agh_precheck() {
    [ -n "$agh_url" ] && [ -n "$agh_pass" ] || return 0
    local code; agh_probe; code=$AGH_CODE
    if [ "$code" != "200" ]; then
        _red "AGH 连接预检失败（HTTP $code）: $agh_url"
        [ "$code" = "401" ] || [ "$code" = "403" ] && _yellow "用户名或密码错误，可用菜单 7 重新配置。"
        agh_fail_hint "$code"
        return 1
    fi
    _green "[ok] AGH 连接正常: $agh_url"
}

prompt_answers() {
    # 非交互模式（stdin 非 tty）且必填项齐全时跳过提问，直接校验。
    if [ ! -t 0 ] && [ -n "$agh_url" ] && [ -n "$agh_pass" ]; then
        _blue "==> 非交互模式，使用命令行参数与默认值"
        validate_answers
        local code; agh_probe; code=$AGH_CODE
        if [ "$code" != "200" ]; then
            _red "AGH 连接预检失败（HTTP $code）: $agh_url"
            [ "$code" = "401" ] || [ "$code" = "403" ] && _yellow "用户名或密码错误。"
            agh_fail_hint "$code"
            exit 1
        fi
        _green "[ok] AGH 连接正常: $agh_url"
        return
    fi
    echo
    _blue_bold "── 1/3 AdGuard Home 连接 ──────────────────"
    # 问完立即连接预检，失败则重新输入（地址 / 凭据错误尽早暴露）。
    local code _p
    while true; do
        if [ -n "$agh_url" ]; then
            ask_str agh_url "AGH 地址（回车沿用已保存地址）" "$agh_url"
        else
            read -rp "AGH 地址（如 http://192.168.1.2:3000）: " agh_url || { _red "输入流已关闭且未提供 AGH 地址"; exit 1; }
        fi
        ask_str agh_user "AGH 用户名" "$agh_user"
        if [ -n "$agh_pass" ]; then
            printf "AGH 密码（回车沿用已保存密码，直接输入则覆盖）: "
            read -rs _p || { _red "输入流已关闭"; exit 1; }; echo
            agh_pass=${_p:-$agh_pass}
        else
            printf "AGH 密码: "
            read -rs agh_pass || { _red "输入流已关闭"; exit 1; }; echo
        fi
        agh_probe; code=$AGH_CODE
        [ "$code" = "200" ] && break
        _red "AGH 连接预检失败（HTTP $code）: $agh_url"
        [ "$code" = "401" ] || [ "$code" = "403" ] && _yellow "用户名或密码错误，请重新输入。"
        agh_fail_hint "$code"
        agh_pass=""   # 地址保留（下次可回车沿用），仅清密码强制重输
    done
    _green "[ok] AGH 连接正常: $agh_url"
    # 带校验的输入统一走临时变量 _v：非法值不写回业务变量，
    # 重输时提示里的 [默认值] 始终是上一个合法值。
    local _v
    while true; do
        ask_str _v "统计窗口（可选 24h/7d/30d/2w）" "$window"
        case "$_v" in 24h|7d|30d|2w) window=$_v; break ;; *) _red "统计窗口非法（仅支持 24h/7d/30d/2w），请重新输入" ;; esac
    done

    echo
    _blue_bold "── 2/3 聚合与调度 ──────────────────────────"
    while true; do
        ask_str _v "点击频次 min-hits（正整数）" "$min_hits"
        if case "$_v" in ''|*[!0-9]*) false ;; *) [ "$_v" -ge 1 ] 2>/dev/null ;; esac; then
            min_hits=$_v; break
        fi
        _red "点击频次必须为 >= 1 的正整数，请重新输入"
    done
    ask_interval || exit 1

    echo
    _blue_bold "── 3/3 CloudflareSpeedTest ────────────────"
    ask_str cfst_dir "cfst 目录" "$cfst_dir"
    while true; do
        ask_str _v "cfst 命令" "$cfst_cmd"
        if validate_cfst_cmd "$_v"; then cfst_cmd=$_v; break; fi
        _red "cfst 命令格式非法（示例：./cfst -tl 200 -dn 20），请重新输入"
    done
    while true; do
        ask_str _v "独立 resolver，逗号分隔（勿指向本机 AGH）" "$resolvers"
        if [ -n "$_v" ]; then resolvers=$_v; break; fi
        _red "resolver 不能为空，请重新输入"
    done

    validate_answers
}

validate_answers() {
    case "$window" in 24h|7d|30d|2w) ;; *) _red "统计窗口非法: $window（支持 24h/7d/30d/2w）"; exit 1 ;; esac
    case "$min_hits" in ''|*[!0-9]*) _red "点击频次必须为正整数: $min_hits"; exit 1 ;; esac
    [ "$min_hits" -ge 1 ] 2>/dev/null || { _red "点击频次必须 >= 1"; exit 1; }
    case "$interval_days$interval_hours" in ''|*[!0-9]*) _red "运行间隔非法（示例: 0 6）"; exit 1 ;; esac
    if [ $((interval_days * 24 + interval_hours)) -lt 1 ]; then
        _red "运行间隔至少 1 小时"; exit 1
    fi
}

total_hours() { echo $((interval_days * 24 + interval_hours)); }

# AGH 连接预检：POST /control/login 拿会话（只读），尽早暴露地址不可达/凭据错误。
# 无 stdout 输出（避免 $( ) 子 shell 吞全局变量），结果写全局：AGH_CODE / CURL_RC / CURL_ERR。
# --noproxy '*'：AGH 属内网直连地址，绕过 shell 代理变量（Clash 等代理会劫持内网请求导致 000，
# 且 systemd/cron 执行主程序时也不带代理环境，此处直连与其行为一致）。
agh_probe() {
    local _tmp
    _tmp=$(mktemp) || { AGH_CODE=000; CURL_RC=1; CURL_ERR="mktemp 失败"; return 0; }
    AGH_CODE=$(curl -sS -m 8 --noproxy '*' -o /dev/null -w '%{http_code}' -X POST \
        "$agh_url/control/login" \
        -H 'Content-Type: application/json' \
        -d "{\"name\":\"$agh_user\",\"password\":\"$agh_pass\"}" 2>"$_tmp") && CURL_RC=0 || CURL_RC=$?
    AGH_CODE=${AGH_CODE:-000}
    CURL_ERR=$(cat "$_tmp" 2>/dev/null || true)
    rm -f "$_tmp"
}

# agh_fail_hint 预检失败时输出可操作的排查信息：curl 退出码映射 + 原始错误 + 手动复测命令。
agh_fail_hint() {
    local code=$1
    [ "$code" = "401" ] || [ "$code" = "403" ] && return 0  # 凭据错误无需连接层诊断
    local reason
    case "$CURL_RC" in
        0)   reason="服务未返回有效 HTTP 响应" ;;
        5)   reason="代理解析失败：环境代理变量指向不可用代理（已尝试绕过仍失败）" ;;
        6)   reason="域名解析失败" ;;
        7)   reason="连接被拒绝：端口未开放或 AGH 未监听该地址（核对端口；若 AGH 仅绑定 127.0.0.1 需换可达地址）" ;;
        28)  reason="连接超时：网络不可达或防火墙拦截" ;;
        35)  reason="TLS 握手失败" ;;
        52)  reason="服务返回空响应：该端口可能是 HTTPS 服务，请把地址改为 https:// 前缀重试" ;;
        60)  reason="证书校验失败：AGH 使用自签名 HTTPS；主程序同样无法信任，请改用 http:// 监听地址或部署受信证书" ;;
        *)   reason="curl 退出码 $CURL_RC" ;;
    esac
    _yellow "  连接层失败: $reason"
    [ -n "$CURL_ERR" ] && _yellow "  curl 原始错误: $CURL_ERR"
    _yellow "  手动复测: curl -v -m 8 --noproxy '*' -X POST '$agh_url/control/login' -H 'Content-Type: application/json' -d '{\"name\":\"用户名\",\"password\":\"密码\"}'"
}

save_settings() {
    cat > "$(settings_file)" <<EOF
INSTALL_DIR=$(sh_quote "$install_dir")
AGH_URL=$(sh_quote "$agh_url")
AGH_USER=$(sh_quote "$agh_user")
AGH_PASS=$(sh_quote "$agh_pass")
WINDOW=$(sh_quote "$window")
MIN_HITS=$(sh_quote "$min_hits")
INTERVAL_DAYS=$(sh_quote "$interval_days")
INTERVAL_HOURS=$(sh_quote "$interval_hours")
CFST_DIR=$(sh_quote "$cfst_dir")
CFST_CMD=$(sh_quote "$cfst_cmd")
RESOLVERS=$(sh_quote "$resolvers")
EOF
    chmod 600 "$(settings_file)"
}

########################################
# 生成 config.yaml / run-opt.sh
########################################
gen_config() {
    local resolvers_yaml="["
    local _sep="" r
    local IFS=','
    for r in $resolvers; do
        r=$(echo "$r" | xargs) # 去首尾空格
        resolvers_yaml="${resolvers_yaml}${_sep}\"$r\""
        _sep=", "
    done
    unset IFS
    resolvers_yaml="$resolvers_yaml]"

    cat > "$install_dir/config.yaml" <<EOF
# cf-opt-adguard 配置（由 install.sh 生成；密码为明文 600 权限，
# 可改为 password: \${AGH_PASSWORD} 形式并在运行环境注入变量）
adguard:
  url: $(yaml_quote "$agh_url")
  username: $(yaml_quote "$agh_user")
  password: $(yaml_quote "$agh_pass")
  timeout: 10s

querylog:
  window: $window
  page_size: 500
  max_pages: 200
  fetch_timeout: 5m

aggregate:
  mode: zone
  min_hits_24h: $min_hits
  min_hits_7d: $min_hits
  max_domains: 1000

detector:
  resolvers: $resolvers_yaml
  concurrency: 8
  timeout: 3s
  http_enabled: true
  score_threshold: 4

cfip:
  source: cfst:$cfst_dir/result.csv
  strategy: lowest_latency
  ip_versions: [4]

sync:
  mode: rewrite-api
  wildcard: true
  ttl: 720h
  rate_limit: 200ms
  retry: 3

runtime:
  db_path: $install_dir/data/state.db
  apply: true
  log_level: info
EOF
    chmod 600 "$install_dir/config.yaml"
}

# 主程序统一调用入口：注入 no_proxy='*' 使其 HTTP 请求与 systemd/cron 定时环境
# （无代理变量）行为一致，避免交互 shell 的 http_proxy 等劫持对内网 AGH 的请求
# （Go 标准 http.Client 默认 ProxyFromEnvironment 会读这些变量）。
run_main() {
    no_proxy='*' NO_PROXY='*' "$install_dir/cf-opt-adguard" "$@"
}

gen_wrapper() {
    local th; th=$(total_hours)
    cat > "$install_dir/run-opt.sh" <<EOF
#!/usr/bin/env bash
# cf-opt-adguard 定时包装：先 CFST 测速 → 再流水线（--apply）。由 install.sh 生成。
set -euo pipefail
INSTALL_DIR=$(sh_quote "$install_dir")
CFST_DIR=$(sh_quote "$cfst_dir")
CFST_CMD=$(sh_quote "$cfst_cmd")
# 手动调试时也不受 shell 代理变量影响（与 systemd/cron 定时环境一致）
export no_proxy='*' NO_PROXY='*'
LOG_DIR="\$INSTALL_DIR/logs"
mkdir -p "\$LOG_DIR"
exec >> >(tee -a "\$LOG_DIR/run.log") 2>&1

echo "=== \$(date '+%F %T') CFST 测速开始（间隔 ${th}h）==="
cd "\$CFST_DIR"
eval "\$CFST_CMD"   # 测速失败即中止，不复用旧 result.csv
echo "=== \$(date '+%F %T') 流水线同步开始 ==="
cd "\$INSTALL_DIR"
./cf-opt-adguard run -c config.yaml --apply
echo "=== \$(date '+%F %T') 完成 ==="
EOF
    chmod 700 "$install_dir/run-opt.sh"
}

########################################
# 定时任务：systemd 优先，回退 cron
########################################
install_schedule() {
    [ "$skip_schedule" = true ] && { _yellow "已跳过定时任务安装（--skip-schedule）"; return 0; }
    if ! is_root; then
        _yellow "非 root，无法安装定时任务；装好后可重新运行本脚本（菜单 4）或执行: sudo bash <(curl -sL $SCRIPT_URL)"; return 0
    fi
    if [ "$SYSTEMD" = 1 ]; then install_schedule_systemd; else install_schedule_cron; fi
}

install_schedule_systemd() {
    local th; th=$(total_hours)
    cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=cf-opt-adguard: CFST 测速 + AdGuard Home rewrite 同步
[Service]
Type=oneshot
ExecStart=$install_dir/run-opt.sh
Nice=10
EOF
    cat > "/etc/systemd/system/${SERVICE_NAME}.timer" <<EOF
[Unit]
Description=cf-opt-adguard 定时触发（每 ${th} 小时）
[Timer]
OnBootSec=10min
OnUnitActiveSec=${th}h
Persistent=true
Unit=${SERVICE_NAME}.service
[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
    systemctl enable --now "${SERVICE_NAME}.timer"
    _green "[ok] systemd timer 已启用（每 ${th} 小时）: systemctl list-timers ${SERVICE_NAME}"
}

# cron 仅支持整除 24 的小时步长与整天步长，其余值向上就近取整并提示。
interval_to_cron() {
    local h=$1 d up
    if [ "$h" -ge 13 ]; then
        d=$(( (h + 23) / 24 )) # 向上取整天
        [ $((d * 24)) -ne "$h" ] && _yellow "提示: cron 按天调度，${h}h 已取整为每 ${d} 天（4 点档）"
        echo "0 4 */$d * *"
        return
    fi
    if [ $((24 % h)) -eq 0 ]; then
        echo "0 */$h * * *"
        return
    fi
    up=12
    for d in 1 2 3 4 6 8 12; do
        if [ "$d" -ge "$h" ]; then up=$d; break; fi
    done
    _yellow "提示: cron 不支持每 ${h} 小时，已就近调整为每 ${up} 小时"
    echo "0 */$up * * *"
}

install_schedule_cron() {
    local spec entry cron_file="/etc/cron.d/${SERVICE_NAME}"
    spec=$(interval_to_cron "$(total_hours)")
    entry="$spec root $install_dir/run-opt.sh >> $install_dir/logs/cron.log 2>&1"
    {
        echo "SHELL=/bin/bash"
        echo "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        echo "$entry"
    } > "$cron_file"
    chmod 644 "$cron_file"
    _green "[ok] cron 已写入 $cron_file（$spec）"
}

schedule_enabled() {
    if [ "$SYSTEMD" = 1 ]; then
        systemctl is-enabled "${SERVICE_NAME}.timer" >/dev/null 2>&1
    elif [ -f "/etc/cron.d/${SERVICE_NAME}" ]; then
        grep -qE '^[0-9*]' "/etc/cron.d/${SERVICE_NAME}"
    else
        return 1
    fi
}

remove_schedule() {
    if is_root; then
        if [ "$SYSTEMD" = 1 ]; then
            systemctl disable --now "${SERVICE_NAME}.timer" 2>/dev/null || true
            rm -f "/etc/systemd/system/${SERVICE_NAME}.timer" "/etc/systemd/system/${SERVICE_NAME}.service"
            systemctl daemon-reload
        fi
        rm -f "/etc/cron.d/${SERVICE_NAME}"
    fi
}

########################################
# 管理菜单快捷命令：保存脚本副本 + 软链到 /usr/local/bin
########################################
manage_sh() { echo "$install_dir/manage.sh"; }

# 把本脚本保存为 $install_dir/manage.sh（curl|bash 场景 $0 为 /dev/fd/*，改从 GitHub 拉取）。
# 先写临时文件再原子替换，避免覆盖"正在运行的本脚本"导致解析错乱。
install_manage_script() {
    local tmp="$(manage_sh).tmp"
    if [[ "$0" != /dev/fd/* ]] && [ -f "$0" ]; then
        cp "$0" "$tmp"
    else
        _blue "==> 保存管理脚本副本（从 GitHub 拉取）"
        curl -fsSL "$SCRIPT_URL" -o "$tmp" || {
            rm -f "$tmp"
            _yellow "管理脚本副本下载失败，跳过快捷命令安装"
            return 1
        }
    fi
    chmod 755 "$tmp"
    mv -f "$tmp" "$(manage_sh)"
}

# root 时软链 /usr/local/bin/cf-opt-adguard -> manage.sh，形成快捷命令，并记录安装位置指针。
install_menu_shortcut() {
    install_manage_script || return 0
    if is_root; then
        ln -sf "$(manage_sh)" "$MANAGE_BIN"
        MANAGE_SHORTCUT_OK=true
        printf "INSTALL_DIR=%s\n" "$(sh_quote "$install_dir")" > "$POINTER_FILE"
        chmod 644 "$POINTER_FILE"
    else
        _yellow "非 root，未创建快捷命令 $MANAGE_BIN；可用: bash $(manage_sh) 打开管理菜单"
    fi
}

remove_menu_shortcut() {
    if is_root; then
        rm -f "$MANAGE_BIN" "$POINTER_FILE"
    fi
}

shortcut_hint() {
    if [ "$MANAGE_SHORTCUT_OK" = true ]; then
        _white_bold "  快捷命令（任意目录可用）:"
        echo "    $APP_NAME               # 打开本管理菜单"
        echo "    $APP_NAME run-sync      # 仅运行同步（用现有测速结果）"
        echo "    $APP_NAME run-once      # 立即跑一轮（分步确认 → 测速 → 同步）"
        echo "    $APP_NAME reconfig      # 修改配置"
        echo "    $APP_NAME help          # 全部子命令"
    else
        _white_bold "  管理菜单（快捷命令未安装）:"
        echo "    bash $(manage_sh)"
    fi
}

# 完成信息键值对：键白色加粗、值绿色，同一行输出。
_kv() { printf "  \033[1;37m%-12s\033[0m\033[0;32m%s\033[0m\n" "$1" "$2"; }

########################################
# 菜单动作
########################################
do_install() {
    # 交互安装第 0 步确认目录（回车默认路径）；非交互或显式 --install-dir 时跳过。
    if [ -t 0 ] && [ "$install_dir_explicit" != true ]; then
        ask_install_dir
    fi
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if [ -d "$install_dir" ] && [ -f "$install_dir/config.yaml" ]; then
        _yellow "检测到已有安装: $install_dir（将保留 settings.env 并重装）"
        load_settings
    fi
    ask_defaults
    prompt_answers

    echo
    _blue "==> 安装目录: $install_dir"
    case "$install_dir" in /opt/*|/etc/*|/usr/*|/var/*)
        is_root || { _red "写入 $install_dir 需要 root，请用 sudo 运行，或 --install-dir 指定用户目录。"; exit 1; } ;;
    esac
    mkdir -p "$install_dir/logs" "$install_dir/data" \
        || { _red "创建目录失败: $install_dir（请检查权限或更换安装目录）"; exit 1; }

    fetch_binary "$install_dir"
    chmod +x "$install_dir/cf-opt-adguard"

    save_settings
    gen_config
    gen_wrapper
    ensure_cfst
    install_schedule
    install_menu_shortcut

    echo
    echo "──────────────────────────────────────────────"
    _green_bold "✅ 安装完成"
    echo "──────────────────────────────────────────────"
    _kv "目录" "$install_dir"
    _kv "配置" "$install_dir/config.yaml"
    _kv "包装" "$install_dir/run-opt.sh（cfst 测速 → run --apply）"
    _kv "间隔" "每 $(total_hours) 小时（${interval_days}d ${interval_hours}h）"
    _kv "日志" "$install_dir/logs/run.log"
    echo
    shortcut_hint
    echo "──────────────────────────────────────────────"
    echo
    if [ -t 0 ] && ask_yesno "是否立即运行一次验证（测速 + 同步）" n; then
        do_run_once || true
    fi
}

# cfst 命令简单校验：首词为 ./cfst / cfst / 绝对路径 */cfst；参数为 - 开头的选项，
# 选项后允许紧跟一个非 - 的值（如 -tl 200）；连续两个值或裸值视为非法。
validate_cfst_cmd() {
    local cmd="$1" first rest="" w prev_opt=0
    [ -n "$cmd" ] || return 1
    first=${cmd%% *}
    [ "$cmd" != "$first" ] && rest=${cmd#* }
    case "$first" in
        ./cfst|cfst|*/cfst) ;;
        *) return 1 ;;
    esac
    for w in $rest; do
        case "$w" in
            -*) prev_opt=1 ;;
            *)
                [ "$prev_opt" = 1 ] || return 1
                prev_opt=0 ;;
        esac
    done
    return 0
}

do_run_once() {
    load_install_dir
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if [ ! -x "$install_dir/cf-opt-adguard" ]; then
        _red "未找到 $install_dir/cf-opt-adguard，请先执行安装（菜单 1）。"
        return 1
    fi
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    cfst_dir=${cfst_dir:-$install_dir/cfst}

    # —— 1) cfst 路径检测 ——
    if [ ! -x "$cfst_dir/cfst" ]; then
        _red "未检测到 CloudflareSpeedTest: $cfst_dir/cfst"
        _yellow "请先用管理菜单「6. 更新 CloudflareSpeedTest」安装，或 --cfst-dir 指定正确目录。"
        return 1
    fi

    # —— 1.5) AGH 前置预检：凭据错误 / 不可达时立即中止，避免测速几分钟白跑 ——
    agh_precheck || return 1

    local interactive=false
    [ -t 0 ] && interactive=true

    # —— 2) 测速命令确认 / 修改（仅交互模式）——
    local final_cmd="$cfst_cmd"
    if [ "$interactive" = true ]; then
        echo
        _blue_bold "── 1/3 CFST 测速 ──────────────────────────"
        echo "  测速目录: $cfst_dir"
        echo "  测速命令: $cfst_cmd"
        local loop=1 pick _cmd
        while [ "$loop" = 1 ]; do
            read -rp "  回车直接运行 / m 修改命令 / q 取消: " pick || return 1
            case "${pick:-y}" in
                y)
                    loop=0 ;;
                m|M)
                    local _cmd
                    while true; do
                        read -rp "  输入 cfst 命令（格式同上，如 ./cfst -tl 200 -dn 20）: " _cmd || return 1
                        if validate_cfst_cmd "$_cmd"; then
                            final_cmd="$_cmd"
                            # 顺手支持保存：改参数大概率想永久生效，免去再进菜单 7。
                            if ask_yesno "  是否保存为默认命令" n; then
                                cfst_cmd="$_cmd"
                                if save_settings; then
                                    _green "  [ok] 已保存为默认命令"
                                else
                                    _red "  保存失败（settings.env 不可写？），仅本次生效"
                                fi
                            fi
                            loop=0
                            break
                        fi
                        _red "  命令格式非法：首词须为 ./cfst、cfst 或以 /cfst 结尾的绝对路径，参数为 - 开头的选项（选项后可跟一个值）；请重新输入"
                    done ;;
                q|Q)
                    _yellow "已取消"
                    return 0 ;;
                *)
                    _red "  无效输入（仅支持 回车 / m / q）" ;;
            esac
        done
    fi

    # —— 3) 运行测速（失败即中止，不复用旧 result.csv）——
    echo "=== $(date '+%F %T') CFST 测速开始: $final_cmd ==="
    _yellow "  测速进行中（视参数通常需要 1-5 分钟），请耐心等待…"
    if ! ( cd "$cfst_dir" && eval "$final_cmd" ); then
        _red "CFST 测速失败，本轮中止。"
        return 1
    fi
    local csv="$cfst_dir/result.csv"
    if [ ! -f "$csv" ]; then
        _red "测速结束但未找到结果文件: $csv"
        return 1
    fi
    _green "[ok] 测速完成，结果: $csv"

    # —— 4) 配置确认（仅交互模式）——
    if [ "$interactive" = true ]; then
        echo
        _blue_bold "── 2/3 同步前确认 ─────────────────────────"
        echo "  配置文件: $install_dir/config.yaml"
        if ! ask_yesno "  配置无误，继续同步" y; then
            _yellow "已取消；可先用菜单 7 重新配置后再运行"
            return 0
        fi
    fi

    # —— 5) 同步：交互终端下 Go 端写入前会列出 CF 域名清单，需再次确认；非交互自动 --yes ——
    echo "=== $(date '+%F %T') 同步开始 ==="
    local rc=0
    if [ "$interactive" = true ]; then
        run_main run -c "$install_dir/config.yaml" --apply || rc=$?
    else
        run_main run -c "$install_dir/config.yaml" --apply --yes || rc=$?
    fi
    echo "=== $(date '+%F %T') 同步结束 ==="
    if [ "$rc" -ne 0 ] && [ "$rc" -ne 5 ]; then
        return "$rc"
    fi
    return 0
}

# 仅运行同步：跳过 CFST 测速，直接用现有 result.csv 写入（交互确认 / 非交互 --yes）。
do_run_sync() {
    load_install_dir
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if [ ! -x "$install_dir/cf-opt-adguard" ]; then
        _red "未找到 $install_dir/cf-opt-adguard，请先执行安装（菜单 1）。"
        return 1
    fi
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    cfst_dir=${cfst_dir:-$install_dir/cfst}
    local csv="$cfst_dir/result.csv"
    if [ ! -f "$csv" ]; then
        _red "未找到测速结果 $csv"
        _yellow "请先运行「立即运行一次」完成 CFST 测速，或检查 --cfst-dir 配置。"
        return 1
    fi
    agh_precheck || return 1
    if [ -t 0 ]; then
        echo
        _blue_bold "── 同步前确认 ─────────────────────────────"
        echo "  配置文件: $install_dir/config.yaml"
        echo "  测速结果: $csv"
        if ! ask_yesno "  使用现有测速结果同步" y; then
            _yellow "已取消"; return 0
        fi
    fi
    echo "=== $(date '+%F %T') 同步开始（跳过测速） ==="
    local rc=0
    if [ -t 0 ]; then
        run_main run -c "$install_dir/config.yaml" --apply || rc=$?
    else
        run_main run -c "$install_dir/config.yaml" --apply --yes || rc=$?
    fi
    echo "=== $(date '+%F %T') 同步结束 ==="
    [ "$rc" -eq 0 ] || [ "$rc" -eq 5 ] && return 0
    return "$rc"
}

# 查看运行日志（run.log 尾部）。
do_logs() {
    load_install_dir
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    local log="$install_dir/logs/run.log"
    if [ ! -f "$log" ]; then
        _yellow "尚无运行日志: $log"
        return 0
    fi
    local n=50
    if [ -t 0 ]; then
        local _v
        while true; do
            ask_str _v "显示最近多少行" 50
            if case "$_v" in ''|*[!0-9]*) false ;; *) [ "$_v" -ge 1 ] 2>/dev/null ;; esac; then
                n=$_v; break
            fi
            _red "行数须为正整数，请重新输入"
        done
    fi
    echo
    _blue "—— $log（最近 $n 行）——"
    tail -n "$n" "$log"
}

# 上次运行记录：systemd 用 timer 的 LastTrigger；否则取 run.log 最后一条标记行。
last_run_info() {
    if [ "$SYSTEMD" = 1 ]; then
        local t
        t=$(systemctl show "${SERVICE_NAME}.timer" -p LastTriggerUSec --value 2>/dev/null)
        if [ -n "$t" ] && [ "$t" != "0" ] && [ "$t" != "n/a" ]; then
            echo "systemd 上次触发: $t"
            return
        fi
    fi
    local log="$install_dir/logs/run.log"
    if [ -f "$log" ]; then
        local last
        last=$(grep -a '=== ' "$log" | tail -n 1 | sed 's/^=== //; s/ ===$//')
        [ -n "$last" ] && { echo "最近运行: $last"; return; }
    fi
    echo "暂无运行记录"
}

# 重新配置运行间隔（定时任务子菜单）：更新 settings / wrapper 并重装定时任务。
do_reschedule() {
    ask_interval || return 1
    validate_answers
    save_settings
    gen_wrapper
    remove_schedule
    install_schedule
    _green "✅ 运行间隔已更新为每 $(total_hours) 小时，定时任务已重载"
}

# 定时任务管理子菜单：状态 + 上次运行，暂停/启用、重配间隔、卸载定时任务。
do_cron() {
    load_install_dir
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    while true; do
        echo
        _blue_bold "── 定时任务管理 ───────────────────────────"
        if schedule_enabled; then
            _green "  状态: 已启用（每 $(total_hours) 小时）"
        else
            _yellow "  状态: 未安装/停用（配置间隔: 每 $(total_hours) 小时）"
        fi
        echo "  $(last_run_info)"
        echo
        echo "  1. 暂停 / 启用 定时任务"
        echo "  2. 重新配置运行间隔"
        echo "  3. 卸载定时任务（保留主程序与配置）"
        echo "  0. 返回主菜单"
        read -rp "  请输入选项: " c || return 0
        case "$c" in
            1) do_toggle || true ;;
            2) do_reschedule || true ;;
            3) remove_schedule; _green "✅ 定时任务已卸载（主程序与配置保留）" ;;
            0|q|Q) return 0 ;;
            *) _red "  无效选项" ;;
        esac
    done
}

do_reconfig() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -d "$install_dir" ] || { _red "未安装: $install_dir"; return 1; }
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    prompt_answers
    save_settings
    gen_config
    gen_wrapper
    ensure_cfst
    if [ "$skip_schedule" != true ]; then remove_schedule; install_schedule; fi
    _green "✅ 配置已更新并重载定时任务"
}

do_toggle() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if ! is_root; then _red "启停定时任务需要 root，请用 sudo 运行。"; return 1; fi
    if schedule_enabled; then
        if [ "$SYSTEMD" = 1 ]; then
            systemctl disable --now "${SERVICE_NAME}.timer"
        else
            sed -i 's/^\([0-9*]\)/# \1/' "/etc/cron.d/${SERVICE_NAME}"
        fi
        _yellow "⏸ 定时任务已暂停"
    else
        if [ "$SYSTEMD" = 1 ]; then
            systemctl enable --now "${SERVICE_NAME}.timer"
        else
            sed -i 's/^# \([0-9*]\)/\1/' "/etc/cron.d/${SERVICE_NAME}"
        fi
        _green "▶ 定时任务已启用"
    fi
}

do_status() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    echo
    if [ -x "$install_dir/cf-opt-adguard" ]; then
        "$install_dir/cf-opt-adguard" version
    else
        _red "未安装或二进制缺失: $install_dir/cf-opt-adguard"
    fi
    echo
    if schedule_enabled; then
        _green "定时任务: 启用"
    else
        _yellow "定时任务: 停用/未安装"
    fi
    if [ "$SYSTEMD" = 1 ] && is_root && systemctl list-unit-files "${SERVICE_NAME}.timer" >/dev/null 2>&1; then
        systemctl list-timers "${SERVICE_NAME}.timer" --no-pager 2>/dev/null || true
    fi
    echo
    local log="$install_dir/logs/run.log"
    if [ -f "$log" ]; then
        _blue "—— 最近日志（tail -n 40 $log）——"
        tail -n 40 "$log"
    else
        _yellow "尚无运行日志: $log"
    fi
}

do_update() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -d "$install_dir" ] || { _red "未安装: $install_dir"; return 1; }
    load_settings
    _blue "==> 更新二进制（保留配置与日志）"
    fetch_binary "$install_dir"
    chmod +x "$install_dir/cf-opt-adguard"
    if is_root && [ "$SYSTEMD" = 1 ] && schedule_enabled; then
        systemctl start "${SERVICE_NAME}.service" || true
    fi
    _green "✅ 更新完成: $("$install_dir/cf-opt-adguard" version)"
}

do_install_cfst() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    cfst_dir=${cfst_dir:-$install_dir/cfst}
    fetch_cfst "$cfst_dir"
}

do_uninstall() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    # 交互模式下高危操作先确认（默认取消），并列出将删除的内容；
    # 非交互（CLI / 脚本调用）保持直接执行。
    if [ -t 0 ]; then
        echo
        _red_bold "⚠ 卸载将删除以下内容："
        echo "  - 定时任务（systemd timer / cron）"
        echo "  - 快捷命令 $MANAGE_BIN 与指针文件 $POINTER_FILE"
        if [ -d "$install_dir" ]; then
            echo "  - 安装目录 $install_dir（含配置、AGH 凭据、状态库与日志）"
        fi
        if ! ask_yesno "确认卸载" n; then
            _yellow "已取消卸载"
            return 0
        fi
    fi
    remove_schedule
    remove_menu_shortcut
    if [ -d "$install_dir" ]; then
        rm -rf "$install_dir"
        _green "✅ 已卸载：定时任务与快捷命令已移除，$install_dir 已删除"
    else
        _yellow "未发现安装目录 $install_dir，仅清理定时任务与快捷命令。"
    fi
}

########################################
# 菜单 / 参数 / 主循环
########################################
menu() {
    load_install_dir
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    # 面板信息全部容错：未安装/无配置也能正常进菜单
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults   # settings 大写变量 → 业务小写变量（间隔显示用）

    # 一行简单状态：版本｜同步间隔｜定时状态｜安装路径
    # version 输出格式 "cf-opt-adguard 0.1.0-dev (linux/amd64, go1.x)"，版本号在第 2 列。
    local ver="-" installed=false
    if [ -x "$install_dir/cf-opt-adguard" ]; then
        ver=$("$install_dir/cf-opt-adguard" version 2>/dev/null | awk '{print $2}')
        ver=${ver:-"?"}
        installed=true
    fi
    local sched
    if schedule_enabled; then
        sched=$(_green "定时已启用")
    else
        sched=$(_yellow "定时未启用")
    fi
    echo
    echo -e "\033[1;36m  ╔══════════════════════════════════════════════╗"
    echo -e "\033[1;36m  ║\033[0m\033[1;37m        cf-opt-adguard 管理菜单\033[0m\033[1;36m               ║"
    echo -e "\033[1;36m  ╚══════════════════════════════════════════════╝\033[0m"
    echo -e "\033[1;37m   $APP_NAME\033[0m $ver ｜ 同步间隔 每$(total_hours)h ｜ $sched ｜ $install_dir"
    if [ "$installed" = true ]; then
        echo "   上次运行: $(last_run_info)"
    fi
    if [ -x "$MANAGE_BIN" ] || [ -L "$MANAGE_BIN" ]; then
        echo -e "\033[0;33m   提示: 任意目录运行 $APP_NAME 均可打开本菜单\033[0m"
    fi

    if [ "$installed" != true ]; then
        echo
        echo -e "\033[1;37m  ── 安装 ───────────────────────────────────\033[0m"
        echo "   1. 安装（二进制 + 配置 + 定时任务 + 快捷命令）"
        echo
        echo "   2. 帮助"
        echo "   0. 退出"
        echo
        read -rp "  请输入选项: " choice || { echo; exit 0; }
        case "$choice" in
            1) action="install" ;;
            2) action="help" ;;
            0|q|Q) exit 0 ;;
            *) _red "  无效选项，请重新输入"; return ;;
        esac
        dispatch "$action" || true
        return
    fi

    echo
    echo -e "\033[1;37m  ── 同步运行 ───────────────────────────────\033[0m"
    echo "   1. 仅运行同步（使用现有测速结果，不测速）"
    echo "   2. 立即运行一次（测速命令确认 → 配置确认 → 同步）"
    echo -e "\033[1;37m  ── 日志与定时 ─────────────────────────────\033[0m"
    echo "   3. 查看运行日志"
    echo "   4. 定时任务管理（状态/上次运行/暂停/重配间隔/卸载定时）"
    echo -e "\033[1;37m  ── 更新 ───────────────────────────────────\033[0m"
    echo "   5. 更新主程序"
    echo "   6. 更新 CloudflareSpeedTest"
    echo -e "\033[1;37m  ── 配置与卸载 ─────────────────────────────\033[0m"
    echo "   7. 重新配置（AGH/窗口/频次/间隔等）"
    echo "   8. 卸载（移除定时任务、快捷命令与安装目录）"
    echo
    echo "   9. 帮助"
    echo "   0. 退出"
    echo
    read -rp "  请输入选项: " choice || { echo; exit 0; }
    case "$choice" in
        1) action="run-sync" ;;
        2) action="run-once" ;;
        3) action="logs" ;;
        4) action="cron" ;;
        5) action="update" ;;
        6) action="install-cfst" ;;
        7) action="reconfig" ;;
        8) action="uninstall" ;;
        9) action="help" ;;
        0|q|Q) exit 0 ;;
        *) _red "  无效选项，请重新输入"; return ;;
    esac
    dispatch "$action" || true
}

parse_args() {
    action="$1"; shift || true
    while [ $# -gt 0 ]; do
        case "$1" in
            --agh-url)     agh_url="$2"; shift 2 ;;
            --agh-user)    agh_user="$2"; shift 2 ;;
            --agh-pass)    agh_pass="$2"; shift 2 ;;
            --window)      window="$2"; shift 2 ;;
            --min-hits)    min_hits="$2"; shift 2 ;;
            --interval)
                if [[ "$2" == *" "* ]]; then
                    interval_days="${2%% *}"; interval_hours="${2#* }"
                else
                    interval_days=0; interval_hours="$2"
                fi
                shift 2 ;;
            --cfst-dir)    cfst_dir="$2"; shift 2 ;;
            --cfst-cmd)    cfst_cmd="$2"; shift 2 ;;
            --resolvers)   resolvers="$2"; shift 2 ;;
            --install-dir) install_dir="$2"; install_dir_explicit=true; shift 2 ;;
            --url)         arg_url="$2"; shift 2 ;;
            --skip-schedule) skip_schedule=true; shift ;;
            --with-cfst)   with_cfst=true; shift ;;
            *) _red "未知参数: $1"; exit 1 ;;
        esac
    done
}

dispatch() {
    case "$1" in
        install)   do_install ;;
        run-sync)  do_run_sync ;;
        run-once)  do_run_once ;;
        logs)      do_logs ;;
        cron)      do_cron ;;
        toggle)    do_toggle ;;
        reconfig)  do_reconfig ;;
        update)    do_update ;;
        install-cfst) do_install_cfst ;;
        uninstall) do_uninstall ;;
        status)    do_status ;;
        help)      usage ;;
        *) usage ;;
    esac
}

usage() {
cat <<EOF
Usage:
  install.sh [command] [options]

安装成功后，任意目录直接运行 $APP_NAME 即可打开管理菜单（等价于本脚本无参数）。

Commands:
  install     安装（二进制 + 配置 + 定时任务 + 快捷命令，缺省进入交互）
  run-sync    仅运行同步（使用现有测速结果，跳过 CFST 测速）
  run-once    立即运行一次（测速命令确认 → 配置确认 → 域名清单确认 → 同步）
  logs        查看运行日志（run.log 尾部）
  cron        定时任务管理（状态/上次运行/暂停/重配间隔/卸载定时）
  reconfig    修改配置并重载定时任务
  toggle      启用 / 暂停 定时任务
  update       更新主程序二进制（保留配置与日志）
  install-cfst 安装 / 更新 CloudflareSpeedTest 到 cfst 目录
  uninstall   卸载（移除定时任务、快捷命令并删除安装目录）
  status      查看版本 / 定时状态 / 最近日志
  help        本帮助

Options:
  --agh-url <url>        AdGuard Home 地址（如 http://192.168.1.2:3000）
  --agh-user <user>      AGH 用户名（默认 admin）
  --agh-pass <pass>      AGH 密码
  --window <w>           统计窗口 24h/7d/30d/2w（默认 7d）
  --min-hits <n>         点击频次阈值（默认 10）
  --interval "<d h>"     运行间隔，天数 小时（如 "0 6"=每6小时, "1 0"=每天；默认 "0 12"）
  --cfst-dir <dir>       CFST 目录（默认 <安装目录>/cfst）
  --cfst-cmd <cmd>       CFST 命令（默认 "./cfst -tl 200 -dn 20"）
  --resolvers <a,b>      独立 resolver（默认 223.5.5.5,119.29.29.29）
  --install-dir <dir>    安装目录（默认 /opt/cf-opt-adguard）
  --url <base>           发布包拉取基址（.../releases/latest/download）
  --skip-schedule        只落文件不装定时任务（测试用）
  --with-cfst            安装时自动拉取 CloudflareSpeedTest 最新版到 cfst 目录

示例:
  sudo bash install.sh                        # 交互菜单
  $APP_NAME                                   # 安装后任意目录打开管理菜单
  $APP_NAME status                            # 查看状态与最近日志
  sudo bash install.sh install --agh-url http://192.168.1.2:3000 \\
      --agh-pass 'xxx' --interval "1 0"       # 非交互安装
EOF
}

detect_env
require_cmds
if [ $# -gt 0 ]; then
    parse_args "$@"
    load_install_dir
    dispatch "$action"
else
    # curl|bash 直管道时 stdin 被下载流占用，无法回答向导，必须引导到进程替换形式。
    if [ ! -t 0 ]; then
        _red "交互式菜单需要终端输入，当前标准输入不是 TTY（可能用了 curl|bash 直管道）。"
        _yellow "请改用进程替换形式（注意是 <(...) 不是 |）:"
        echo "    bash <(curl -sL $SCRIPT_URL)"
        echo "  或先下载再执行:"
        echo "    curl -sL $SCRIPT_URL -o /tmp/cf-opt-install.sh && sudo bash /tmp/cf-opt-install.sh"
        exit 1
    fi
    load_install_dir
    while true; do
        menu
    done
fi
