# 流媒体分发核心（本地两档片段发布）

用 Go 实现的本地流媒体分发起步工程：把自带短媒体编码为两档 fMP4 片段，
发布带时间信息的片段索引，本地客户端只在可衔接边界切换档位，并用
ffmpeg/ffprobe 实际解码核对声音与画面既不重复也不缺段。不依赖任何云分发服务。

## 工程保存的三类资产

- 源素材：source/video.y4m（纯 Go 生成，灰度随时间线性变化的画面）、
  source/audio.wav（纯 Go 生成的 24kHz 线性扫频）、source/source.mp4
  （ffmpeg 合流编码后的源）。画面亮度与声音瞬时频率都直接编码时间，便于
  解码后核对内容是否与时间一致。
- 片段索引：publication/current.json 与不可变的
  publication/versions/manifest.vN.json，记录每档每个片段的 start_pts、
  时长、帧数、采样数和文件路径。
- 发布版本：每版片段独立存放于 publication/media/vN/tier/，发布通过
  暂存区加原子替换 current.json 完成；发布失败或取消时上一版保持可用。

## 可衔接切换的规则

- 源为 8s、10fps，片段固定 2s，共 4 段；两档（low 160x120、high
  320x240）由同一次 ffmpeg 运行在统一时间轴上切出，关闭 B 帧重排，
  切点严格落在 0/2/4/6s。
- 发布前逐段探测：必须有完整的视频帧数量（每段 20 帧）、音频流与正确的
  帧时间窗；跨档同序号片段的首帧 PTS 必须一致。未完成或损坏片段导致整个
  发布中止，不进入索引。
- 客户端 Switch 只接受索引中的片段边界；片段中间的切换请求被拒绝，
  因为那必然重复或丢掉一段音视频。播放计划逐段校验 PTS 连续、无重复。

## 实际解码核对

play 与集成测试会对计划中的每个片段（用对应档位的 init）实际解码：

- 画面：解码为灰度原始帧，逐帧比较平均亮度与源素材在该 PTS 应有的亮度，
  并核对总帧数恰为 80，少了即缺段、多了即重复。
- 声音：解码为 24kHz 单声道 PCM，按 AAC 分段 priming 对齐后拼接，逐
  100ms 窗口估计瞬时频率并与扫频期望比较，覆盖全部档位拼接边界；总采样
  数恰为 192000。

## 运行

    go run ./cmd/streamcore generate -workdir work
    go run ./cmd/streamcore encode   -workdir work
    go run ./cmd/streamcore publish  -workdir work
    go run ./cmd/streamcore play     -workdir work \
      -start low -switch high@2 -switch low@4 -switch high@6

测试（含真实 ffmpeg 的端到端流水线）：

    go test ./...

## 代码结构

- internal/source：纯 Go 生成自带音视频素材并合流。
- internal/encode：一次 ffmpeg 运行产出两档时间对齐的 fMP4 片段。
- internal/fftool：ffmpeg/ffprobe 封装与探测结果类型。
- internal/manifest：片段索引模型与时间轴、跨档边界校验。
- internal/publish：逐段探测、暂存、不可变版本与原子发布。
- internal/client：连续读取与仅允许边界切档的播放计划。
- internal/verify：实际解码后的画面内容、声音内容与帧数、采样数核对。
- internal/it：真实端到端测试（生成、编码、发布、切档、解码核对，以及
  未完成片段拒绝发布且保留上一版）。

后续接入实时输入时，只需让实时源沿用相同的 PTS 时间单位、2s 分片边界和
完成判定，发布索引与客户端切档逻辑无需改变。
