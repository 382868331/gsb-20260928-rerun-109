# 本地流媒体分发核心

这是一个纯 Go、无云依赖的本地多档媒体分发起步工程。它把源素材、片段索引和发布版本分开保存，后续实时输入可以继续复用同一套 48 kHz 时间基和片段 PTS。

## 存储布局

- `sources/short/base`、`sources/short/sharp`：两档短媒体源片段。
- `sources/short-index.json`：只包含解码成功、校验完整的片段；`.part` 或未完成片段不会进入索引。
- `publish/versions/<id>/index.json`：不可变发布索引，记录片段边界和 SHA-256。
- `publish/versions/<id>/<variant>/`：发布版本实际读取的片段副本。
- `publish/staging-<id>/`：待提交发布；调用取消会删除，已发布版本保持不变。

片段格式为本地 `.sm` 容器：文件头记录尺寸、声道、时间基、起始 PTS 和时长；随后交错保存视频 RGB 帧与 16-bit PCM 音频。所有视频帧都是关键帧，片段起点可独立解码。

## 连续切换

两档内容共享同一条时间线，每个片段时长为 48000 ticks（1 秒）。播放器只允许在片段边界切换，切换后从目标档位同一 PTS 的片段继续读，因此不会重复或跳过音频/画面帧。

发布提交采用暂存目录加目录替换：新版本完整复制并校验通过后才替换旧版本；取消发布只清理暂存区，上一版仍可读取。

## 直播窗口

`internal/live` 在 `live/<stream>/index.json` 维护固定长度滑动窗口，片段位于 `live/<stream>/<variant>/000000.sm` 一类路径。新片段先写 `.part`，完整生成并校验后原子改名，随后才替换清单，因此暂存片段不会出现在可播放窗口中。恢复发布时以清单的 `next_sequence` 继续编号，不会重复时间戳。

播放器获得一个播放租约和当时的不可变窗口快照，只能按连续序号读取，并可在任意片段边界切换到同序号的其他档位。若发布推进导致当前序号滑出窗口，读取返回可判别的 `ErrWindowExpired`，不会拼接后续片段；调用方必须显式 `Jump`/`JumpToLive`，或调用 `Stop`。窗口内回看使用相同的 PTS 和序号校验。

从窗口退役但仍被旧播放者租约引用的文件会保留在发布目录中，只有所有旧租约释放后才随下次窗口更新清理。进程重启代表旧播放者已不存在，启动时会清理遗留的退役文件和 `.part` 暂存片段。

## 运行

```bash
go test ./...
go run ./cmd/streamctl demo -root /tmp/localstream-demo -id v1
go run ./cmd/streamctl read -root /tmp/localstream-demo -variant base -switch-segment 2 -switch-to sharp
go run ./cmd/streamctl publish-live -root /tmp/localstream-demo -stream news -window 3 -segments 6
go run ./cmd/streamctl read-live -root /tmp/localstream-demo -stream news -window 3 -jump-live
```

测试会真实生成并解码短媒体，逐像素核对画面、逐采样核对声音，并检查完整时间线没有重叠或缺口。
