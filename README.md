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

## 运行

```bash
go test ./...
go run ./cmd/streamctl demo -root /tmp/localstream-demo -id v1
go run ./cmd/streamctl read -root /tmp/localstream-demo -variant base -switch-segment 2 -switch-to sharp
```

测试会真实生成并解码短媒体，逐像素核对画面、逐采样核对声音，并检查完整时间线没有重叠或缺口。
