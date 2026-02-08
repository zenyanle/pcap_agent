# 1. 使用 Ubuntu 24.04 作为干净的底座
FROM ubuntu:24.04

# 2. 基础配置 (Root 阶段)
ENV DEBIAN_FRONTEND=noninteractive

# 3. 安装 Homebrew 必须的系统级依赖 (只用 apt 装这些)
# build-essential: 编译工具
# procps: Homebrew 需要 ps 等命令
# curl/git/file: 下载和脚本运行必须
RUN apt-get update && apt-get install -y \
    build-essential \
    procps \
    curl \
    file \
    git \
    locales \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# 4. 配置语言环境 (Homebrew 强制要求 UTF-8，否则会报错)
RUN localedef -i en_US -f UTF-8 en_US.UTF-8
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

# 5. 创建 linuxbrew 用户并赋予 sudo 权限
# Homebrew 强烈建议不要用 root 运行
RUN useradd -m -s /bin/bash linuxbrew && \
    echo 'linuxbrew ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

# --- 以下操作全部以 linuxbrew 用户身份执行 ---
USER linuxbrew
WORKDIR /home/linuxbrew

# 6. 安装 Homebrew
# 设置禁止自动更新以加快构建速度
ENV HOMEBREW_NO_AUTO_UPDATE=1
RUN /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# 7. 将 Homebrew 加入 PATH 环境变量
# 这样后续的 brew install 才能直接运行
ENV PATH="/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:${PATH}"

# 8. 使用 Brew 安装核心工具 (Zeek, Wireshark, Nmap, UV)
# 注意：
# - wireshark 包里已经包含了 tshark
# - brew install uv 会直接安装最新版 uv
# - 安装 zeek 时间可能稍长，请耐心等待
RUN brew install \
    zeek \
    wireshark \
    nmap \
    uv \
    python@3.12

# 9. 使用 uv 配置 Python 分析环境
# 创建一个虚拟环境 venv
ENV VIRTUAL_ENV=/home/linuxbrew/venv
RUN uv venv $VIRTUAL_ENV

# 将虚拟环境加入 PATH，这样输入 python 直接调用 venv 里的
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# 10. 使用 uv 极速安装 Python 库
RUN uv pip install \
    pyshark \
    scapy \
    pandas \
    ipython \
    requests \
    jupyter

# 11. 设置工作目录
WORKDIR /data

# 默认进入 bash
CMD ["/bin/bash"]
