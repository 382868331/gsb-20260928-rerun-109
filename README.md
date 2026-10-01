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

## 持续直播窗口

`internal/live` 使用本地生成器持续追加各档位片段。每个片段仍为 1 秒、48000 ticks，所有档位的新清单共享同一个 `window_start_pts` 和 `timeline_duration`；恢复发布时从最新已提交版本读取时间线，不会重复已经提交的 PTS。

新片段先写入隐藏暂存目录，全部档位复制和 SHA-256 校验成功后才提交不可变版本并更新流指针，`.part`、隐藏临时文件和未完成 staging 都不会出现在可播放清单中。播放器打开版本时会在 `publish/refs/<version>/` 建立引用；窗口滚动只清理没有引用的旧版本，仍在播放的旧索引和文件保持可读。调用 `Player.Close` 释放引用后，后续发布才会删除旧版本。

播放器固定在打开的清单版本上，调用 `Refresh` 才会切到当前清单。若播放位置已经落在新窗口外，会返回 `ErrWindowExpired`，旧清单不会被悄悄拼接；播放者可以调用 `JumpToLive` 明确跳到最新片段，或者继续读完旧版本后收到 `ErrPlaybackEnded` 停止。`JumpTo` 用于在当前窗口内回看，只接受窗口内片段边界。

## 运行

```bash
go test ./...
go run ./cmd/streamctl demo -root /tmp/localstream-demo -id v1
go run ./cmd/streamctl read -root /tmp/localstream-demo -variant base -switch-segment 2 -switch-to sharp
go run ./cmd/streamctl live -root /tmp/localstream-live -stream news -window 4 -segments 8
go run ./cmd/streamctl read-live -root /tmp/localstream-live -stream news -variant base -start-pts 192000 -switch-to sharp
```

测试会真实生成并解码短媒体，逐像素核对画面、逐采样核对声音，并检查完整时间线没有重叠或缺口。
