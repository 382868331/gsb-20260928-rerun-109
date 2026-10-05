# 本地流媒体分发核心

这是一个纯 Go、无云依赖的本地多档媒体分发起步工程。它把源素材、片段索引和发布版本分开保存，后续实时输入可以继续复用同一套 48 kHz 时间基和片段 PTS。

## 存储布局

- `sources/short/base`、`sources/short/sharp`：两档短媒体源片段。
- `sources/short-index.json`：只包含解码成功、校验完整的片段；`.part` 或未完成片段不会进入索引。
- `publish/versions/<id>/index.json`：不可变发布索引，记录片段边界和 SHA-256。
- `publish/decisions/<id>.json`：不可变发布决定，独立于可回收媒体保存，记录发布类型、更正/撤回缘由、涉及片段、每个质量档的前后校验值、父发布以及内容采用的来源版本。
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

## 窗口内内容更正

已经公布但仍在窗口中的片段可以更正（替换错误画面、修正音量）：`Publisher.Correct(segment, reason)` 为所有质量档生成替换片段，校验通过后作为同一个不可变版本原子提交，任一环节失败都保留完整旧版，流指针不移动。更正版本与上一版共享窗口边界，只有被更正片段的 SHA-256 不同。每个更正版本的决策 JSON 都记录更正缘由、涉及片段、各档前后 SHA-256、更正序号和内容采用关系。

同一区间可以连续更正。每次更正都是新版本，操作者可用 `Publisher.CompareReleases(from, to)` 或 `streamctl compare-releases` 比较任意两个仍保留版本，得到每个片段在每个质量档的前后校验值及跨档合并区间。播放器可用 `Publication` 查看当前版本，用 `ReleaseNote` 查看当前发布决定，用 `AdoptedReleaseID(variant, segment)` 追溯当前读取内容实际生成于哪次发布。

撤回不会修改旧版。`Publisher.Withdraw(correctionID, reason)` 或 `WithdrawCorrection(segment, reason)` 会生成新的不可变发布决定，把撤回区间的内容恢复到被撤回更正的父版本，并在新说明中记录撤回目标和采用来源。仍持有旧版本的播放器继续读取旧文件和旧索引；只有主动 `AdoptLatest` 的播放器才会看到撤回后的新决定。连续更正可逐层撤回：撤回第二版会回到第一版，再撤回第一版才回到原始内容。

正在播放旧版本的播放者固定在旧版本上，不会在片段中途混入新内容；新打开的播放者直接得到更正后的当前版本。播放者调用 `AdoptLatest` 主动采用更正：播放位置保持在同一内容时刻接续，返回值中的 `[]store.Range` 标明与之前版本不同的区间（基于各档校验和比较、跨档合并）。若播放位置已移出新窗口，`AdoptLatest` 返回 `ErrWindowExpired` 且保持旧版本，播放者可继续读完旧版本（`ErrPlaybackEnded` 停止）或 `JumpToLive` 明确跳转。

重开发布者时从最新已提交版本恢复时间线、版本序号和更正序号；发布决定链不随媒体回收删除，因此重开后仍可解释更正缘由、区间、校验值和采用关系，并比较任意两次历史发布。窗口过期与引用释放规则不变：完全滚出窗口且没有 `publish/refs/<version>/` 引用的版本索引和媒体文件才会清理；有播放器持有的旧媒体保持可读，释放后再推进窗口即可回收，不通过永久保留媒体维持历史说明。

## 运行

```bash
go test ./...
go run ./cmd/streamctl demo -root /tmp/localstream-demo -id v1
go run ./cmd/streamctl read -root /tmp/localstream-demo -variant base -switch-segment 2 -switch-to sharp
go run ./cmd/streamctl live -root /tmp/localstream-live -stream news -window 4 -segments 8
go run ./cmd/streamctl correct -root /tmp/localstream-live -stream news -segment 5 -reason "fix lower third"
go run ./cmd/streamctl releases -root /tmp/localstream-live -stream news
go run ./cmd/streamctl compare-releases -root /tmp/localstream-live -stream news -from news-00000000000000000004 -to news-00000000000000000006
go run ./cmd/streamctl withdraw-correction -root /tmp/localstream-live -stream news -segment 5 -reason "correction rejected"
go run ./cmd/streamctl read-live -root /tmp/localstream-live -stream news -variant base -start-pts 192000 -switch-to sharp
```

测试会真实生成并解码短媒体，逐像素核对画面、逐采样核对声音，并检查完整时间线没有重叠或缺口。
