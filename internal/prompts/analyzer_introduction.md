## 1. 环境概述 (System Context)
这是一个基于 **Ubuntu 24.04** 的 Docker 容器，专用于网络流量分析 (PCAP) 和取证。
- **系统架构**: Linux x86_64
- **当前用户**: linuxbrew (非 root，但有无密码 sudo 权限)
- **包管理器**: Homebrew (系统级), uv (Python 级)
- **工作目录**: /data (建议将 PCAP 文件挂载至此目录)
- **Python环境**: 虚拟环境已自动激活 (/home/linuxbrew/venv)

## 2. 常用工具指南 (Tool Usage)

### A. Tshark (Wireshark 命令行版)
用于精确提取包信息或进行包过滤。

* **基本读取**:
  tshark -r input.pcap
* **应用过滤器 (显示过滤器)**:
  tshark -r input.pcap -Y "http.request.method == POST"
* **提取特定字段 (CSV格式)**:
  tshark -r input.pcap -T fields -e frame.number -e ip.src -e ip.dst -e http.host
* **统计分析**:
  tshark -r input.pcap -q -z io,phs (协议分级统计)

### B. Zeek (原 Bro)
用于将 PCAP 文件转换为结构化的日志文件 (conn.log, http.log, dns.log 等)。

* **分析 PCAP 文件**:
  zeek -r input.pcap
  *(注意：这会在当前目录下生成大量 .log 文件)*
* **查看连接日志**:
  cat conn.log | zeek-cut id.orig_h id.resp_h service
* **指定脚本策略**:
  zeek -r input.pcap frameworks/files/extract-all-files (提取流量中的文件)

### C. Python 分析库 (已预装)
环境使用 uv 管理依赖，虚拟环境默认激活。直接运行 python 或 ipython 即可。

#### 1. Scapy (强大的包伪造与解析)
from scapy.all import *
# 读取 PCAP
packets = rdpcap("input.pcap")
# 查看摘要
packets.summary()
# 访问特定层 (例如提取 DNS 查询)
for pkt in packets:
if DNS in pkt and pkt[DNS].qr == 0:
print(pkt[DNS].qd.qname)

#### 2. PyShark (Tshark 的 Python 封装)

import pyshark
# 懒加载读取 (适合大文件)
cap = pyshark.FileCapture('input.pcap', display_filter='http')
for pkt in cap:
print(pkt.http.host)

#### 3. Pandas (数据统计)

通常结合 CSV 使用。先用 Tshark 导出为 CSV，再用 Pandas 分析。