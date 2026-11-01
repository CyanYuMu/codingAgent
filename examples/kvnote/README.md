# kvnote 并发验收样例

这是从真实 Agent 编程任务保留下来的 Go 标准库样例，用于回归验证 JSON 数据保护和跨进程更新。

```sh
go run ./examples/kvnote --file /tmp/notes.json set greeting hello
go run ./examples/kvnote --file /tmp/notes.json get greeting
go run ./examples/kvnote --file /tmp/notes.json list
go run ./examples/kvnote --file /tmp/notes.json import other.json
go run ./examples/kvnote --file /tmp/notes.json del greeting
go test -race ./examples/kvnote
```

存储只接受 JSON 字符串对象。缺失文件视为空对象，损坏文件和非法导入不会被覆盖；新数据文件权限为 0600。退出码为 0（成功）、1（运行错误）、2（参数错误）。

所有 `set`、`del`、`import` 都在读取目标之前获取稳定的 `<数据文件>.lock` 排他锁，并持有到临时文件写入、同步、关闭和 rename 完成。锁文件必须保留，删除它会产生两个锁 inode；仅对最后的 rename 或被替换的 JSON inode 加锁都不足以防止丢失更新。测试同时启动八个独立进程，覆盖 set、del 和 import。

当前使用 macOS/Linux 的 advisory flock，所有写入者必须采用本协议；外部编辑器不会被自动锁住。写入不接受最终路径为符号链接。文件同步不等同于完整的断电恢复保证。本样例不是 Agent 核心存储，也不承诺任意网络文件系统上的锁语义。
