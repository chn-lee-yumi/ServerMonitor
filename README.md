# ServerMonitor

Scribe server statistics and write to influxdb.

通过netlink获取网卡流量和Qdisc信息，并写入到influxdb。

直接使用命令`cat /proc/net/dev`和`tc -s qdisc`获取到的信息是一样的。