#!/bin/sh
# AetherLink 容器入口。
#
# 目标：宿主上什么都不用准备、compose 里也不用配任何东西，
# 挂一个目录进来就能起。
#
# 绑定挂载（./config:/config）时宿主目录的属主会覆盖镜像里的设置，
# 通常是 root:root。容器内的非 root 进程无法在其中创建配置文件，
# 主进程启动即失败，配合 restart: always 就表现为容器反复重启。
# 所以这里先把属主修好，确认目标用户真的写得进去，最后才降权。
set -e

config_path="${AETHERLINK_CONFIG:-/config/config.yaml}"
config_dir=$(dirname "$config_path")
puid="${PUID:-10001}"
pgid="${PGID:-10001}"

# can_write 用指定身份实打实地建一个临时文件，而不是去解析 stat 的权限位：
# NFS、SMB、各家 NAS 的 ACL 都可能让权限位与真实结果不符。
can_write() {
    su-exec "$1:$2" touch "$config_dir/.aetherlink-write-probe" 2>/dev/null || return 1
    rm -f "$config_dir/.aetherlink-write-probe"
    return 0
}

if [ "$(id -u)" != "0" ]; then
    # compose 里指定了 user:，没有改属主的权限，只能提示后照常启动。
    if [ ! -w "$config_dir" ]; then
        echo "[entrypoint] 警告：$config_dir 对当前用户 $(id -u):$(id -g) 不可写"
        echo "[entrypoint] 请在宿主上执行 chown -R $(id -u):$(id -g) 映射到 $config_dir 的目录，"
        echo "[entrypoint] 或去掉 compose 里的 user: 让入口脚本自动处理"
    fi
    exec /aetherlink "$@"
fi

mkdir -p "$config_dir" 2>/dev/null || true
# 无条件 chown：目录里只有配置文件，代价可以忽略。
# 这同时修好了历史遗留的 root 属主配置文件（早期版本以 root 跑过的情况）。
chown -R "$puid:$pgid" "$config_dir" 2>/dev/null || true

if can_write "$puid" "$pgid"; then
    exec su-exec "$puid:$pgid" /aetherlink "$@"
fi

# 属主改不动但 root 写得进去：部分 NAS 的 SMB/NFS 挂载、带强制 ACL 的存储池会这样。
# 这时宁可以 root 跑也不要陷入重启循环——至少界面能打开、配置能保存。
if can_write 0 0; then
    echo "[entrypoint] 警告：$puid:$pgid 无法写入 $config_dir，改以 root 运行"
    echo "[entrypoint] 这会让配置文件归 root 所有。想以普通用户运行，请在宿主上执行："
    echo "[entrypoint]   chown -R $puid:$pgid <宿主上映射到 $config_dir 的目录>"
    exec /aetherlink "$@"
fi

# 连 root 都写不进去，通常是把 /config 挂成了只读。继续启动只会反复失败退出，
# 不如把原因一次说清楚。
echo "[entrypoint] 错误：$config_dir 必须可写，但连 root 都写不进去"
echo "[entrypoint] AetherLink 把管理口令与上游配置保存在 $config_path，没有可写目录无法运行。"
echo "[entrypoint] 请检查 compose 的卷定义有没有写成只读（结尾的 :ro），例如应当是："
echo "[entrypoint]   volumes:"
echo "[entrypoint]     - ./config:/config"
exit 1