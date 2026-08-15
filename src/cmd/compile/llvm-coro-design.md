# 基于 Go 官方编译器的 LLVM Coroutine 自动染色设计

状态：架构设计稿；已完成 Phase 0、pre-lower handoff、受限可执行 LLVM coroutine
basic example、单函数 Go object/archive/external-link ownership、push scheduler、native
timer event、blocking file executor handoff，以及 connected stream socket 纵切；typed
structured await、runtime timer/file/net adapter 和普通标准库 lowering 尚未接入

更新时间：2026-08-15

官方镜像线：`cpunion/go:main`（保持与 Go 官方 `master` 一致）

长期开发线：`cpunion/go:dev.coro`（只 merge `cpunion/go:main`）

当前 implementation topic/worktree：`dev.coro-socket-operation-20260815`
（向 `cpunion/go:dev.coro` 提交）

## 1. 调研基线

本文回答一个具体问题：如果不再以 `x/tools/go/ssa` 为主要前端，而是基于 Go
官方编译器的 Unified IR 和中端，实现 LLGo `llvm-coro` 分支所设计的自动异步染色、
structured await、抢占和动态函数值处理，编译流水线应如何组织。

本次分析基于以下版本：

- `dev.coro` 起点 / Go 官方 `master`：
  `6db72bb92b2ab681ae177589b70b573e6e337b96`
- `cpunion/llgo:llvm-coro` 初始设计审查：
  `7227d1c832cd12a7a4c85cb42cc3f4fe5d8c6343`
- `cpunion/llgo:llvm-coro` 工程量统计：
  `9a0de9cb0db0945e40241b1d67cf36eac83bcaa7`
- `xgo-dev/llgo:main`：`56d8423700bedca3574e649372ea97c980fbbc27`

重点参考了 `llvm-coro` 分支的四份文档：

- [统一异步执行核心与扩展契约](https://github.com/cpunion/llgo/blob/llvm-coro/doc/coro-async-core-contract.md)
- [Callable 与调用点契约](https://github.com/cpunion/llgo/blob/llvm-coro/doc/coro-callable-contract.md)
- [Coroutine 语义标准化 IR 与统一 Lowering](https://github.com/cpunion/llgo/blob/llvm-coro/doc/coro-ir-design.md)
- [LLVM Coroutine Runtime 总体设计](https://github.com/cpunion/llgo/blob/llvm-coro/doc/llvm-coro-runtime-design.md)

本文把“源码已经确认的事实”和“建议采用的设计”分开描述。Go `master` 会继续变化；
`cpunion/go:dev.coro` 通过 merge 跟进，以上 commit 是首次实现和复核的固定起点；
`cpunion/go:main` 不携带 coroutine patch。

### 1.1 当前 Phase 0 PoC

`dev.coro` 已在上述 Go 官方 `master` 起点实现一个最小、默认关闭的前端 PoC：

- 增加 `GOEXPERIMENT=coro` 和 `-d=coro={1,2}`；默认构建继续使用 upstream Unified IR
  V4，实验构建使用 V5。
- 在 interleaved devirtualization/inlining 前执行 provisional analysis，在 ABI wrapper
  生成后、escape 前执行 final analysis。
- 从 Unified IR 识别 channel send/receive/range/select、direct/defer/go call 和
  dynamic/interface unknown call。
- 复用官方 `ir.VisitFuncsBottomUp`，在递归 SCC 内计算 `NoSuspend`/`MaySuspend`
  两点 fixed point；`defer` 传播 effect，`go` target 不反向染色 caller。
- 通过 versioned Unified IR function extension 导出、导入跨包 effect；不带实验身份的
  archive 会被拒绝。
- 增加三包 `leaf -> mid -> root` 测试，证明 effect 经过真实 package archive 传播，
  最终程序仍由官方 native backend 构建并运行。
- provisional result 用于观察内联前的 effect 和 call edge；final analysis 对内联后
  IR 重新染色。Phase 0 尚未改变 ABI，也没有冻结 SitePlan，因此不提前限制正常内联。
- 增加 `ssa.CompileWithLoweringHook`：在 generic SSA pass 完成、首个 target `lower`
  前调用可替代 backend；hook 声明 handled 时 native lowering 不再运行。当前
  `-d=coro=3` consumer 只报告 blocks/values 并继续 native backend。
- 增加 `-d=corobasic=<file.ll>` 的窄纵向例子：从同一个 pre-lower hook 验证固定
  scalar SSA recipe，生成可独立执行的 LLVM coroutine module；Go 函数本身仍继续
  native lowering，这个输出不是 Go object。

截至 2026-07-26 的实测结果如下：

| 验证 | 结果 |
| --- | --- |
| 默认 `make.bash` / `all.bash` | 通过 |
| 默认 `go test cmd/compile/...` | 通过 |
| `GOEXPERIMENT=coro make.bash`，工具链版本含 `X:coro` | 通过 |
| 实验模式 `coro`、`ssa`、`pkgbits`、`buildcfg`、`noder` 聚焦测试 | 通过 |
| 三包 summary round trip、mixed archive negative、native executable smoke | 通过 |
| 本机 LLVM 19.1.7 上 LLGo `go test ./ssa -run TestCoro` | 通过 |
| 实验模式完整 `go test cmd/compile/...` | 通过 |
| basic module 的 pre/post-CoroSplit verify、link、suspend/resume/destroy | 通过 |

初版 PoC 曾直接把 provisional `MaySuspend` 复用为 `Noinline`/call-site `NoInline`。
这使开放函数值、interface call 或缺 summary 的 bodyless call 保守染色后阻止正常
内联，导致现有 inliner golden 失配。它不是 Phase 0 的正确约束：当前没有 coroutine
ABI 或稳定 SitePlan 消费这些 barrier，final analysis 又能在内联后重新发现被复制到
caller 的 channel seed。删除提前 barrier 并增加 inlining compatibility test 后，
完整实验 compiler suite 通过。Phase 1 真正改变 ABI 时，应基于稳定 SiteID、已闭合
candidate set 和明确的 plan ownership 增加窄 barrier，不能恢复按 unknown effect
全局禁止内联的做法。

这个 PoC 已验证 Phase 0 和第 19 节中“pre-lower SSA 能进入 LLVM”以及“LLVM
coroutine 能真实 suspend/resume/destroy”的窄 basic case，但仍不满足完整
executable vertical slice 的 go/no-go 条件。它尚未实现：

- stable FunctionID、SiteID、summary digest、primary ABI、FuncRep、Demand 或 SitePlan；
- compiler-owned `YieldOnce`/`Await` semantic op；
- alternate backend 从 ssagen 到 object/link 的完整 ownership handoff；
- 通用 LLVM IR emitter、Go object/link ownership、scheduler/runtime 和跨包
  coroutine codegen；
- coroutine ABI、GC/debug metadata 或标准库/runtime 兼容。

对 Go `master` 的代码路径复核确认：`ssagen.buildssa` 建立 generic SSA 后立即调用
`ssa.Compile`，而其静态 pass list 随后进入 arch-specific `lower`、schedule 和
regalloc。PoC 为第一个 `lower` pass 增加显式 boundary marker 和窄
`CompileWithLoweringHook` API；单元测试验证 hook 看到 generic `Add64`、尚未 schedule
或 regalloc，并验证 continue-native 后该 op 已 lowering。默认 `Compile` 仍走原路径。

handoff 回答了“能否在正确时点交出 generic SSA”；basic example 进一步证明这个
输入能生成并运行 LLVM switched-resume coroutine。当前 ssagen consumer 仍是
side-artifact 模式，若 hook 返回 handled 会主动报错，因为 `ssagen.Compile` 后续仍
假设 native `AllocFrame`、`genssa` 和 object emission。下一个 vertical slice 需要把
handled 结果提升为明确的 backend ownership，并由 LLVM emitter 提供 object、symbol
与 linker 所需产物；不能在最终机器 SSA 后反译 LLVM。

### 1.2 可执行 basic LLVM 纵向例子

固定输入位于 `cmd/compile/internal/coro/testdata/basic.go`。它只包含一个 `int64`
参数、一次 suspend、一个跨 suspend 使用的 scalar result，以及 suspend 前后的两个
全局自增。pre-lower recognizer 只接受以下闭合 recipe：

```text
single Ret block
  InitMem
  before = load(before) + 1
  StaticCall(yieldOnce)
  after = load(after) + 1
  result = int64 argument + constant
  MakeResult(result, memory)
```

当前 `yieldOnce` 通过私有 testdata 中的精确 link-symbol suffix 识别。这是为了把
SSA handoff 到 LLVM execution 的机械路径压缩到最小而采用的临时限制，不是第 8、9
节所要求的最终 SiteID/typed semantic op。任何额外 block、第二次 marker call、
其他 call/load/store 或不匹配的 memory chain 都 fail closed。

生成模块包含一个 `presplitcoroutine` 的 `leaf` 和一个独立 `main` driver。driver
逐项验证：

1. 首次调用停在 initial suspend，`before == 1`、`after == 0`、result 尚未发布，
   且 `coro.done == false`。
2. 调用 `coro.resume` 后到达 final suspend，`before` 仍为 1、`after == 1`、
   `leaf(40) == 43`，且 `coro.done == true`。
3. 调用 `coro.destroy`，确认 destroy cleanup marker 已写入并释放 frame，最后输出
   `coro-basic-ok`。

本机验证使用 LLVM 19.1.7：

```text
GOEXPERIMENT=coro ./bin/go tool compile -l -p=basic \
  -d=corobasic=/tmp/basic.ll -o /tmp/basic.o \
  src/cmd/compile/internal/coro/testdata/basic.go

opt -S -passes=coro-early,coro-split,coro-cleanup,verify \
  /tmp/basic.ll -o /tmp/basic.split.ll
clang -O1 /tmp/basic.split.ll -o /tmp/basic-coro
/tmp/basic-coro
```

`TestBasicLLVMExecution` 固化了同一流程，并检查 split 后存在 `leaf.resume`、
`leaf.destroy` 和 coroutine frame；Linux CI 显式安装 LLVM 后运行，不能以缺少工具
为由跳过。`basic_llvm.go` 的 recognizer、negative branches、writer 和 renderer
另有直接单元测试，文件内各函数 statement coverage 均为 100%。

这个例子明确不做三件事：

- `-d=corobasic` 不是影响 ABI 的主开关；主开关仍只有 `GOEXPERIMENT=coro`。
- 生成的 `.ll` 是 standalone side artifact，原 Go function 仍由 native backend
  生成 object。
- driver 直接调用 resume/destroy，不代表 scheduler、Go runtime、跨包 coroutine
  ABI 或 linker ownership 已经完成。

### 1.3 2026-08-15 upstream merge checkpoint

Integration commit `44875ea4a4` 在不重放 coroutine 提交的前提下，把 Go 开发版
`41a1646f6d` merge 进 `dev.coro`。这 172 个提交只产生两个内容冲突，均来自官方 SSA
入口新增的 `*ssa.HTMLWriter` 参数。解决方案保留官方 `Compile(f, htmlWriter)` API，
并把同一个 writer 传入可选的 pre-lower hook；没有用旧 compiler 文件覆盖新版本。

merged source 在 native Darwin/arm64 上通过以下门禁：

| 验证 | 结果 |
| --- | --- |
| 默认 `make.bash` | 通过 |
| 默认 `go test cmd/compile/... internal/pkgbits internal/buildcfg` | 通过 |
| `GOEXPERIMENT=coro make.bash` | 通过 |
| 实验模式完整 compiler、pkgbits 和 buildcfg 测试 | 通过 |
| LLVM 19 pre/post-CoroSplit verify 和可执行 basic example | 通过 |

可执行例子仍是 standalone side artifact。本次 merge 不改变第 19 节的完成度：Go
object/archive/link ownership 和 reentry scheduler 仍是下一个必需纵切。仓库分支策略
现在明确为：`main` 镜像 upstream，topic PR 以 `dev.coro` 为目标，只有经过 review 的
upstream-merge topic 才更新这条长期分支。

### 1.4 受限 Go object/archive/link ownership 纵切

`dev.coro-object-link-20260815` 在 upstream merge topic `7b8b1f264e` 之上实现了第一个
不再保留 Go native primary 的 LLVM object/link 例子。它有意只接受以下函数：

```go
//go:noinline
func leaf() int64 {
    yieldOnce()
    return 42
}
```

函数必须无参数、只返回一个常量 `int64`，并且 generic SSA 中只有一个
`yieldOnce` marker call 和一个 `Ret` block。零参数限制避免在这个物理链路验证中把
Go ABIInternal 输入寄存器与 host C ABI 混在一起；当前两个 ABI 的单个 scalar 返回值
在 Darwin/arm64 与 Linux/amd64 上可以直接衔接。完整 coroutine ABI 仍由第 10 节定义，
不能从这个巧合外推。

“返回值可以衔接”不表示函数入口可以直接共用 host ABI。Linux/amd64 的端到端测试
曾在 LLVM 生成的第一个 `movaps` 上失败：Go ABIInternal 调用点没有提供该 System V
函数假定的栈对齐，并且 System V 也不保证保留 Go ABIInternal 用作零寄存器的 XMM15。
受限 wrapper 因此使用 LLVM `alignstack(16)` 自行重对齐，并在所有返回边清零 XMM15。
这是当前无参数 PoC 的最小 ABI bridge；参数、栈增长、async preemption、GC stack map
和阻塞调用的 M handoff 仍必须由第 10、11、12 节的正式 adapter 解决。

纵切的 ownership 流程是：

```text
prepareFunc 识别受限 candidate，不建立 native text body
  -> pre-lower generic SSA recognizer 最终 fail-closed 校验
  -> LLVM coroutine module -> clang native object
  -> compiler archive: __.PKGDEF + _go_.o + co<48-bit digest>.o
  -> _go_.o private coro manifest 映射 Go symbol/ABI 到 host symbol
  -> cmd/pack 展开 compiler archive，并原名保留 native object member
  -> linker 校验 manifest version/target/duplicate symbol
  -> Go symbol 标为 SHOSTOBJ，自动选择 external link
```

成员名的摘要包含 Go symbol 和 ABI，使用 48 bit，并恰好适配 Go archive 的 16-byte
short-name 限制。manifest 位于新包 `cmd/internal/coroobj`，由 compiler 与 linker
共享；它限制输入大小、拒绝未知 JSON 字段、重复 Go/host symbol、控制字符、错误 ABI
和错误 target。显式 `-linkmode=internal` 会以稳定原因拒绝，而不是等到 host object
relocation 才失败。

Darwin/arm64 上的端到端测试已经证明：编译器输出 `action=emit-native-object`，archive
中同时存在 Go object、空 marker assembly object 和 coroutine native object，`go tool
nm` 能看到 host definition，最终 executable 输出 `coro-object-ok`。测试还覆盖
`cmd/pack` 的 compiler-archive 展开、experiment gate、unsupported SSA、重复 artifact、
manifest 编解码和 linker `SHOSTOBJ` 映射。新增 manifest package 的 statement coverage
为 94.0%；`basic_object.go` 除操作系统临时目录/空 object 等故障注入分支外均有直接
测试。

这个纵切仍有四个刻意保留的边界：

- `-d=coroobject=<clang>` 是 debug PoC selector；compiler 内调用 clang，cache identity
  只包含路径而不包含工具内容，不能作为正式 backend driver。
- 本纵切最初使用函数内同步 wrapper；后续第 1.5 节已把同一受限 object 例子替换为
  显式 scheduler/operation reentry，但仍没有真实 timer/I/O event source、parent await
  或并发 worker。
- object 使用 `malloc/free`，host external-link 启动路径由测试显式导入
  `runtime/cgo`；这不代表直接 C/no-cgo boundary 已完成。
- marker 仍按私有符号名识别，尚未替换成 typed `YieldOnce`、SiteID 和版本化 callable
  fact。
- 当前 module contract 已在 LLVM 19.1.7 验证，并兼容 LLVM 19–21 的 `i1
  llvm.coro.end`；LLVM 22 把该 intrinsic 改为 `void`，需要 versioned intrinsic ABI
  renderer。当前不能把 LLVM 22 verifier 失败误报为 object/link ownership 失败。

因此该结果只把风险从“Go compiler 能否把 LLVM object 交给 archive/linker”降为已
验证；它没有提前完成第 19 节的 scheduler/runtime 验收。

### 1.5 Push scheduler 与 operation reentry 纵切

`dev.coro-scheduler-operation-20260815` 叠加在 object/link topic 上，把同步 resume
wrapper 替换为最小的 push FIFO 和显式 operation 生命周期。生成的 LLVM module 创建
两个 logical task；每个 task 都依次发生 initial suspend、`Yield`、`Park` 和完成，
scheduler 记录并校验 resume 顺序必须是 `0,1,0,1,0,1`。

operation 使用 `(task, generation, phase)` 标识，phase 单向经过
`Idle -> Armed -> Parked/Ready -> Consumed`。task 0 在 arm 后、提交 park 前完成，验证
early completion 不丢 wakeup；task 1 在 ready queue 变空后完成，验证 late publisher
只唤醒精确 waiter。重复入队、错误 task/generation、非法 phase 和非 runnable task 都
fail closed。两个 task 都只在 scheduler 中 resume/destroy，coroutine body 只提交
action，不递归驱动 scheduler。

Darwin/arm64 上使用 LLVM 19.1.7 的真实 compile/archive/external-link executable 已
通过，并返回原 Go 函数期望值。这个结果证明了单线程下 push queue、early/late
completion 与 LLVM coroutine reentry 的物理闭环；它尚未证明跨线程发布、内存序、
timer/netpoll/file adapter、M handoff 或完整 runtime 公平性。下一纵切应保持相同
operation contract，只把手工 late publisher 替换为 timer event source。

### 1.6 非阻塞 timer event source 纵切

`dev.coro-timer-operation-20260815` 保留第 1.5 节的 queue/operation contract，并把
task 1 的手工 late completion 替换为真实 POSIX timer thread。task 1 提交 park 后，
scheduler 才创建 timer thread；timer thread 等待另一个 stackless task 至少推进 8 次，
随后执行 1 ms `nanosleep`，记录系统调用结果，并以 release store 发布 ready。scheduler
每次 dispatch 边界用 acquire load 观察 ready，之后仍由 scheduler 自己调用
`operation.publish`。timer thread 从不直接修改 ready queue、task state 或 operation
phase。

task 0 仍覆盖 early completion，consume 后在 timer ready 前反复 `Yield` 并递增 atomic
progress。最终 executable 同时校验 progress、timer syscall、thread join、两个 operation
的 `Consumed` phase、FIFO 前四次 resume 顺序、空队列和两个 Go 返回值。正常路径和失败
清理路径都会 join 已创建的 thread，避免 scheduler frame 退出后仍有 event source 引用
其中的状态。

Darwin 的 `pthread_t` 是 pointer，Linux 64-bit 是 integer，因此 renderer 显式按
`GOOS/GOARCH` 选择 ABI，并对其他 target fail closed。该 slice 已在 Darwin/arm64 的
LLVM 19.1.7 和 Linux/amd64 的 LLVM 18 上通过
compile、CoroSplit、archive、external link 和 executable 测试。

这证明了“timer 阻塞留在 event thread，stackless scheduler 继续推进 ready task”的
最小物理路径，但还不是生产 `time.Sleep`：当前每个例子只有一个 thread、scheduler 在
dispatch 边界 polling、没有 deadline heap/parking primitive、没有 `time.Sleep` 自动
染色或 runtime timer API adapter。下一步应把同一 release/acquire publication contract
接到可 park 的 timer service，再让普通 Go `time.Sleep` call site 进入该 operation。

### 1.7 阻塞 file read 与 executor handoff 纵切

`dev.coro-file-operation-20260815` 增加第二个私有 object recipe，不改变 timer recipe。
它创建空 POSIX pipe，让调用线程直接执行阻塞 `read`；在进入 `read` 前，只把 push queue
的所有权以 release store 交给一个 replacement pthread。replacement 用 acquire load
取得所有权后，反复 resume 同一个 sibling stackless task。sibling 至少推进 8 次后直接
`write` 一个字节，唤醒原调用线程。replacement 退出并被 join 后，原线程才把 owner
切回并继续 scheduler mutation，因此两个 executor 不会并发修改 queue、task state 或
operation phase。

scheduler、tasks、operation 和 file handoff 状态放在同一 native heap object；跨线程
字段只通过原子 publication 共享。最终 executable 校验两个 pthread 身份不同、精确的
单字节 read/write、progress、join、queue 空状态、两个返回值，以及阻塞 task 的
early-completion operation 最终进入 `Consumed`。LLVM module 直接调用 libc
`read`/`write`，不经过 cgo trampoline；测试中的空 assembly marker 只用于触发这个受限
recipe，不是用户源码注解或长期自动染色接口。

这仍不是生产 Go M handoff。replacement pthread 没有注册成 runtime M，不能执行依赖
P、G、GC 或 Go stack 的任意代码；当前 no-argument recipe 也没有连接 `os.File.Read`。
生产路径应保留阻塞调用所在的原 M，通过与 `reentersyscallblock`/`exitsyscall` 等价的
runtime adapter 把 P 或 stackless scheduler ownership 交给可复用 replacement M，避免
退化为每次调用创建 worker 的模型。该物理纵切只验证 handoff 的控制流、queue
single-owner 约束和 direct native call ABI。

该 slice 已在 native Darwin/arm64 和 Linux/arm64 上通过 coroutine toolchain build 与
完整 `cmd/compile/... internal/pkgbits internal/buildcfg` 测试；Linux/amd64 的 timer 和
blocking-file archive/link/executable 用例各连续通过三次。GitHub Ubuntu runner 负责
native Linux/amd64 完整 suite 的最终门禁。新增 recipe 选择和 renderer 的 changed Go
functions 均被测试覆盖。

### 1.8 Connected stream socket 与 executor handoff 纵切

`dev.coro-socket-operation-20260815` 复用第 1.7 节的 blocking-FD scheduler 和所有权
协议，但用 `socketpair(AF_UNIX, SOCK_STREAM)` 建立 connected stream socket，并把直接
`read`/`write` 换成 libc `recv`/`send`。调用线程在空 socket 上阻塞 `recv`；replacement
pthread 独占 push queue，至少推进 sibling stackless task 8 次后由 sibling `send` 一个
字节释放调用线程。最终检查仍要求线程身份不同、精确单字节 I/O、operation consumed、
ownership 归还、replacement join 和空 queue。

file 和 socket recipe 共用同一份 scheduler IR 模板，只把 FD 初始化和 I/O call 作为
受限 recipe 参数，避免复制 queue、operation 和 M-handoff 状态机。`AF_UNIX` 与
`SOCK_STREAM` 在当前支持的 Darwin/Linux target 上均为 1；其他 target 仍由统一 target
gate fail closed。

这个 slice 验证阻塞 socket call 不占住唯一 stackless executor，但不是 TCP 或 production
netpoll。它没有 listener/connect/accept、nonblocking retry、deadline/cancel、poll token、
DNS 或 `net.Conn.Read` lowering。下一阶段必须把同一 operation publication contract 接到
runtime netpoll，并用 loopback TCP、deadline 和关闭竞态验证一次完成与取消语义。

该 slice 已在 native Darwin/arm64 和 Linux/arm64 上通过 coroutine toolchain build 与
完整 compiler suite；Darwin 上的 timer/file/socket archive/link/executable 路径各连续
通过五次，Linux/amd64 上各连续通过三次。新增及修改 renderer 的 changed Go functions
均为 100% statement coverage。

## 2. 结论

方案可行，推荐复用或 fork Go 官方编译器的 parser、types2、Unified IR、内联、
逃逸分析前端和机器无关 SSA 优化，在 `walk` 之前完成 coroutine 语义分析，在
机器相关 `lower` 之前转入 LLVM emitter。

不能采用以下实现：

```text
Go 官方编译器生成机器 SSA
    -> 根据 runtime 函数名猜测哪些调用会挂起
    -> 把最终 SSA 翻译为 LLVM coroutine
```

原因有三点：

1. `walk` 已把 channel、select、go、defer 等语言结构展开成 runtime 调用和临时变量，
   原始控制语义、求值边界和调用点身份已经弱化。
2. 机器相关 `lower`、寄存器分配和栈布局针对 Go 原生后端，不适合作为 LLVM
   CoroSplit 的输入。
3. coroutine 染色会改变调用 ABI、函数值表示、逃逸和 frame 生命周期，无法在普通
   object 已生成后由 linker 补做。

推荐的总流水线是：

```text
源码
  -> syntax/types2/Unified IR
  -> 导入依赖包 CoroSummary
  -> provisional effect（约束内联）
  -> devirtualize + inline + wrappers
  -> ProgramModelBuilder 固定点
       - Effect / Exec
       - FuncRep / value-flow
       - Demand / emission
       - SitePlan / callable facts
  -> 冻结 CoroProgramIR
  -> coroutine-aware escape
  -> walk（保留少量 coroutine semantic family）
  -> Go generic SSA
  -> 机器无关优化
  -> LLVM IR emitter
  -> LLVM CoroSplit
  -> frame/GC/debug metadata
  -> LLGo coroutine runtime 与平台 adapter
```

同一开发分支和同一套 toolchain 必须同时提供两种模式：

- 默认关闭 coroutine，完整保留 Go 官方 compiler、linker、runtime 和 ABI 路径。
- 显式开启 `GOEXPERIMENT=coro` 后，才启用自动染色、coroutine ABI、LLVM backend 和
  对应 runtime。

关闭开关时不是“尽量兼容”，而是必须持续通过 upstream 全部测试并保持官方语义。开启
开关时首先兼容 Go 源码、package graph、`go build/test` 工作流和标准库语义；普通 Go
archive 与 coroutine archive 因 ABI 不同，初期必须重编或在 link 时拒绝混用。

这里“基于 Go 官方编译器”首先表示复用成熟的语言前端和中端，不表示可以原样复用
Go runtime、原生 goroutine 栈、汇编 ABI 和最终机器码后端。后四项是另一组更大的
兼容工程。

## 3. 从 `llvm-coro` 设计继承的约束

### 3.1 LLVM coroutine 不是异步运行时

LLVM coroutine intrinsic 只帮助保存、恢复和销毁 stackless continuation。它不提供：

- goroutine 调度；
- channel、timer、netpoll 或 host Promise 语义；
- 取消和 operation 生命周期；
- panic、defer、recover、Goexit 的跨 frame 传播；
- Go pointer 的保根、pin、copy 或精确扫描。

编译器必须先把 Go 程序转换成明确的逻辑 continuation 和 operation 协议，再由
LLVM 生成和拆分物理 frame。

### 3.2 一个源码函数只有一个 primary body

- `NoSuspend` 函数只生成 plain primary。
- `MaySuspend` 或需要可挂起抢占的函数只生成 coroutine primary。
- coroutine caller 调用 coroutine callee 时生成 structured await。
- 同步 ABI consumer 调用 coroutine 函数时只生成薄 root/reentry adapter。
- 不为同一源码函数复制 plain 和 coroutine 两份完整 CFG。

这项约束控制代码体积，也避免两份 body 的 panic、debug、coverage 和优化语义漂移。

### 3.3 三种分析必须独立

| 分析 | 回答的问题 | 产物示例 |
| --- | --- | --- |
| Effect / Exec | 函数是否可挂起、是否需抢占、在哪类 executor 运行 | `MaySuspend`、`NeedsPreempt`、affinity |
| FuncRep / value-flow | 函数值能否保持直接表示，还是必须动态分派 | `DirectPlain`、`DirectCoro`、`Dispatch` |
| Demand / emission | 当前链接单元实际需要哪些入口和 adapter | primary、descriptor、root adapter |

`MaySuspend` 不等于“函数值必须是 descriptor”；`BothDemand` 也不等于“生成两份
函数体”。

### 3.4 Emitter 不得重新分析源码

可达函数、hidden helper、effect、动态调用候选、frame retention 和每个挂起点的
协议必须由一个 builder 固定下来。LLVM emitter 只能消费冻结的计划，不能再次扫描
raw IR 并独立判断“这个调用看起来像 timer/channel/syscall”。

## 4. Go 官方编译器提供的插入点

### 4.1 当前主流水线

Go 官方编译器的主要顺序可从
[`cmd/compile/internal/gc/main.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/gc/main.go)
确认：

1. `noder.LoadPackage` 产生类型检查后的 Unified IR。
2. coverage、PGO 和 loop 标记完成。
3. `interleaved.DevirtualizeAndInlinePackage` 执行去虚化与内联。
4. `noder.MakeWrappers`、loopvar、init task 和 ABI wrapper 产生额外函数。
5. `escape.Funcs` 执行逃逸分析。
6. 每个函数在 `prepareFunc` 中执行 `walk.Walk`。
7. `ssagen` 构造并优化官方 SSA，最终生成目标机器码。

[`prepareFunc`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/gc/compile.go)
明确显示 `walk.Walk(fn)` 紧靠 SSA 构造之前。因此最终 coroutine 计划必须在
`walk` 之前冻结。

### 4.2 为什么需要两个分析时点

只在内联之后分析太晚：内联器可能已经把 `MaySuspend` callee 当作普通函数内联，
破坏唯一 primary body、独立 coroutine frame 和 cleanup owner。

只在内联之前分析也不够：去虚化、内联、method wrapper、ABI wrapper、loopvar 和
init task 会改变本包最终调用图。

建议分两阶段：

1. **Provisional analysis**

   建议插在 `bloop.Walk` 之后、`interleaved.DevirtualizeAndInlinePackage` 之前。
   此时读取 imported summary，扫描本包 suspend seed，求一次保守 effect 固定点。
   第一阶段把 `MaySuspend`、未知动态调用和相关 wrapper 标为不可按普通规则内联。

2. **Final analysis**

   建议插在 `pkginit.MakeTask`、`symABIs.GenABIWrappers` 之后，
   `deadlocals.Funcs`、`escape.Funcs` 之前。在去虚化、允许的内联、wrapper 和 init
   task 生成之后，对最终 Unified IR 求完整固定点并冻结 `CoroProgramIR`。

初始版本应采用简单可靠的内联规则：

- `NoSuspend` 可内联进 plain 或 coroutine caller。
- `MaySuspend` 不内联。
- 未知或 summary 不匹配的 imported callee 按 `MaySuspend` 处理。

以后可以增加 coroutine-aware inlining，但必须保留 suspend site identity、defer
owner、debug inline tree 和 frame 成本模型，不能直接打开普通内联。

### 4.3 晚生成函数必须进入同一个闭包

官方流水线仍有晚于 package-level escape 的函数生成路径。例如 Wasm
import/export wrapper、reflect/type metadata 所需 wrapper 和部分 bodyless intrinsic
会在 enqueue 或 prepare 阶段才物化。若按现状直接在 escape 前一次性冻结 package，
这些函数会绕过 coroutine 分析。

实现时必须选择并验证以下一种策略：

1. 把所有可能包含 managed call、suspend 或函数值边界的 body 生成提前到 final
   analysis 之前；或
2. 让每个 late generator 调用同一个 `ProgramModelBuilder.RegisterGeneratedFunc`，
   在 escape/walk 前完成 seed、effect、FuncRep、Demand 和 verifier。

第二种策略只允许追加满足以下条件的函数：

- effect 完全由已冻结 target summary 和 generator contract 推导；
- 不向已冻结函数引入新的反向 effect edge；
- 新 demand 只能单调增加 descriptor/adapter；
- 新 wrapper 在进入 `walk` 前已有完整 FunctionPlan 和 SitePlan。

如果 generator 不能证明这些条件，就必须提前物化，不能由 LLVM emitter 临时补做。
这与 `llvm-coro` 分支要求 helper/reachable closure 先稳定、再冻结
`CoroProgramIR` 的规则一致。

### 4.4 官方 SCC 工具可以直接复用

[`cmd/compile/internal/ir/scc.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/ir/scc.go)
已经提供 `VisitFuncsBottomUp` 和 Tarjan SCC 遍历。本包 effect 固定点可以以此为基础，
对递归调用簇一次 join，避免另建一套函数身份和闭包遍历。

## 5. 自动染色模型

### 5.1 不用一个枚举承载所有含义

推荐的函数 summary 是正交维度，而不是不断扩大的
`Sync/Async/Preempt/Foreign/Host/...` 枚举：

```go
type CoroSummary struct {
    Suspend SuspendEffect
    Exec    ExecFlags
    Cost    PreemptCost
    Rep     FuncRepSummary
    ABI     CoroABI
    Digest  [32]byte
}

type SuspendEffect uint8

const (
    NoSuspend SuspendEffect = iota
    MaySuspend
)

type ExecFlags uint32

const (
    NeedsPreempt ExecFlags = 1 << iota
    NeedsScheduler
    NeedsForeignWorker
    NeedsHostLoop
    ThreadAffine
    RealmAffine
    MayUnwind
)
```

primary body 的选择是派生规则：

```text
MaySuspend || NeedsPreempt -> coroutine primary
otherwise                  -> plain primary
```

这样，纯计算但需要抢占的循环、会 park 的 channel 操作、只能在特定 host realm 恢复的
调用可以共享 continuation 后端，同时保留不同 executor 约束。

### 5.2 Seed 来源

最终应从语义节点或版本化 callable fact 产生 seed，不按符号名猜测：

- channel send、receive 和 select；
- scheduler yield、park、sleep、timer；
- 可能异步完成的 foreign/host operation；
- coroutine child await；
- 会跨 frame 传播的 panic、defer、recover、Goexit 控制边；
- 需要 stackless 抢占的循环、递归 SCC 和未知长计算；
- bodyless assembly、C 或 host declaration 的显式 contract。

编译器 directive 可以用于声明和校验，例如 `nosuspend`、`bounded`、
`thread_affine`，但不能把未经证明的普通函数强行降成 `NoSuspend`。违反声明时应编译
失败。

### 5.3 调用边传播

| 调用边 | 对 caller 的传播 | 对 callee 的 demand |
| --- | --- | --- |
| 直接普通调用 | 传播 callee effect | 当前调用方式 |
| `defer f()` | 传播 callee effect 和 unwind 能力 | 当前 frame cleanup |
| `go f()` | 不把等待 effect 传播给 caller | 新的 async scheduler root |
| 闭合集合的动态调用 | join 全部候选 | direct set 或 descriptor |
| 开放动态调用 | 保守 `MaySuspend` | `Dispatch` |
| hard-sync ABI boundary | 不允许普通传播 | 需要 root adapter 或诊断 |

`go f()` 是最容易误染色的边：它使 `f` 必须有 scheduler 可启动入口，但启动动作本身
不等待 `f`。`defer f()` 则相反，它仍属于当前逻辑调用链，若 deferred call 可挂起，
cleanup frame 也必须可挂起。

### 5.4 固定点

对于本包每个函数 `F`：

```text
Effect(F) =
    LocalSeed(F)
    join Effect(each direct/defer managed callee)
    join DynamicFallback(F)
    join PreemptionSeed(F)
```

以 SCC 为单位从叶到根计算，单调 lattice 保证收敛。Go package import graph 无环，
因此 imported package summary 可以作为已冻结叶节点；本包不需要 whole-program
call graph 才能完成基本染色。

但是 linker 无法修正已经生成的 caller ABI。如果缺少 imported summary，只有两种
正确做法：

- fail closed，把调用当作开放 `MaySuspend` 并使用动态 ABI；
- 拒绝混合 archive，并要求用同一 coroutine toolchain 重编。

不能先生成 plain call，再期待 linker 发现 callee 是 coroutine 后补一个 await。

### 5.5 高阶函数

简单的常量 effect 对高阶 API 过于保守。例如一个 imported 函数的 effect 可能是：

```text
Effect(Map) = LocalEffect(Map) join Effect(callback parameter 0)
```

建议跨包 summary 后续支持受限的 effect expression：

- `Effect(param[i])`
- `join`
- 闭合 method/interface candidate set
- 明确的 `go param[i]`，只产生 demand 不传播等待

第一阶段不实现表达式时，应把 imported 高阶调用保守归为 `MaySuspend + Dispatch`；
不能用“常见 callback 不阻塞”作为正确性依据。

### 5.6 抢占也会染色 caller

官方 Go 的 loop reschedule check 可以依赖 goroutine native stack；stackless coroutine
不能在 plain caller 的 native activation 仍在栈上时挂起。因此：

- 循环、递归 SCC、未知成本调用路径可能产生 `NeedsPreempt`。
- `NeedsPreempt` 与 `MaySuspend` 一样沿同步调用链向上传播。
- poll 只能放在 compiler 证明的 safepoint。
- poll 的 suspend edge 必须经过 compiler-owned resume gate。

Go 官方 SSA 当前在通用优化末尾、机器 `lower` 前执行
`insertLoopReschedChecks`，详见
[`cmd/compile/internal/ssa/compile.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/ssa/compile.go)。
LLVM coroutine 路径应禁用这条原生 runtime check，改为生成 CoroProgramIR 已计划的
`Poll`。

## 6. 跨包 summary

### 6.1 存放位置

Unified IR 已区分公开 object 数据和 compiler-private extension：

- [`noder/writer.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/noder/writer.go)
- [`noder/reader.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/noder/reader.go)
- [`noder/linker.go`](https://github.com/golang/go/blob/6db72bb92b2ab681ae177589b70b573e6e337b96/src/cmd/compile/internal/noder/linker.go)

推荐把 coroutine summary 放进 compiler-private object extension，而不是公开 export
data。这样 `go/types` consumer 不需要理解 coroutine ABI，编译器 importer 却能在
内联和本包染色前取得精确事实。

### 6.2 最小 schema

每个 exported、address-taken 或可能出现在 inline body 中的函数至少记录：

```text
SchemaVersion
ToolchainVersion
Stable FunctionID
SuspendEffect
ExecFlags
PreemptCost / UnknownCost
PrimaryKind
parameter/result FuncRep schema
effect expression（可选）
callable contract dependencies
physical ABI/layout version
summary digest
```

Package-level header 还应记录 coroutine experiment、runtime ABI、pointer width、target
capability 和 LLVM coroutine ABI 版本。

### 6.3 缓存与混合模式

- summary 必须进入 export hash 和 build action identity。
- linker 校验 producer/consumer 的 schema、ABI 和 target digest。
- 普通 `gc` archive 与 coroutine archive 默认不允许静默混用。
- 插件、shared library、cgo、assembly 和 linkname 需要单独 compatibility gate。
- summary 缺失、版本未知或 hash 不一致时 fail closed。

whole-program cache digest 可以发现不一致，但不能替代 producer archive 中的 summary，
因为 caller 在 package 编译阶段已经需要知道调用 ABI。

## 7. FuncRep 与动态调用

### 7.1 直接调用不需要 descriptor

编译期知道唯一目标时：

```text
DirectPlain -> 普通 call
DirectCoro  -> child continuation + structured await
```

只有函数值进入开放或 ABI-visible 边界时才 canonicalize 为 descriptor，例如：

- interface、`any` 和 reflect；
- global、heap、map、channel 或未知 aggregate storage；
- 跨 package/archive 的开放函数值；
- callback registry；
- 无法闭合候选集合的动态调用。

### 7.2 Descriptor 原则

descriptor 可以含有：

- stable FunctionID；
- plain 或 coroutine primary slot，二者按函数 effect 互斥；
- context/closure data；
- ABI 和 signature digest；
- 必要的 boundary adapter capability。

descriptor 不得通过 code pointer tag 或“地址反查函数属性”获得 effect，也不得发布两份
完整 source body。

第一阶段可以保守规定：任何进入 memory、interface、reflect、export 或未知调用边界的
函数值都使用 `Dispatch`。后续再用官方 IR 的 value-flow 缩小 descriptor 范围。

### 7.3 必须单独解决的元数据

以下能力不能只靠改 direct call：

- method value 和 method expression；
- interface itab method slot；
- generic instantiation 和 wrapper；
- `reflect.Value.Call`、`MakeFunc`；
- cgo callback；
- assembly function pointer；
- `FuncPCABI0` 和调试/符号接口。

它们需要统一的前向 callable fact。禁止由裸 `uintptr` 反推出 coroutine 类型。

## 8. CoroProgramIR

### 8.1 单一冻结事实源

推荐在 `cmd/compile/internal/coro` 新增一个 package-level `ProgramModelBuilder`，输出：

```go
type Program struct {
    Functions []FunctionPlan
    Calls     []CallPlan
    Sites     []SitePlan
    Types     []FuncRepPlan
    Imports   []ImportedSummary
    Schema    Schema
    Digest    [32]byte
}
```

`FunctionPlan` 包含 Effect、Exec、Demand、primary ABI、cleanup/outcome、preemption 和
frame policy；`SitePlan` 是稀疏计划，只记录需要 coroutine lowering 的源位置。

builder 的工作顺序是：

```text
导入 summary 和本包 roots
  -> 扫描高层 IR，产生 provisional sites/calls/helpers
  -> helper/wrapper/reachable closure 固定点
  -> Effect/Exec SCC 固定点
  -> FuncRep/value-flow 固定点
  -> Demand/emission 固定点
  -> 冻结 identity、schema、digest
  -> verifier
```

后续 escape、walk、SSA builder 和 LLVM emitter 都通过稳定 ID 查询同一份计划。

### 8.2 SitePlan 最小语义族

只保留少量通用 family：

| Family | 含义 |
| --- | --- |
| `Await` | 等待 managed child continuation |
| `Park` | 等待 scheduler/runtime operation |
| `ForeignOp` | 可能阻塞或异步完成的 C/syscall |
| `HostOp` | 返回 host event loop 的 Promise/JSPI 等 |
| `Spawn` | 新 scheduler root |
| `Poll` | 抢占安全点 |
| `Cleanup` | defer/panic/recover/Goexit outcome |

`time.Sleep`、fd read、network、RTOS notification 不应各自变成新的 compiler opcode。
它们应由 typed operation recipe 描述参数、prepare、park、resume、result lease 和
cancel。

### 8.3 控制边

每个 suspend site 至少记录：

- source position 和稳定 SiteID；
- 求值只执行一次的 operand；
- normal、suspend、resume、cancel、panic successor；
- resume gate 类型；
- operation/result lifetime；
- 跨 suspend live value；
- cleanup owner；
- target capability。

标准 protocol 由 emitter 的模板生成物理 LLVM blocks，计划不复制完整普通 CFG。

### 8.4 Verifier

冻结后、生成 LLVM 前至少校验：

- plain primary 的可达路径不存在 suspend；
- 每个 coroutine call site 的 callee ABI 与 summary 一致；
- 每个 suspend 有唯一 continuation 和合法 resume gate；
- 所有 operation 在 resume/cleanup 上完成 take、discard 或 cancel；
- frame-borrow 没有越过声明的 lifetime；
- unknown dynamic call 只能走 Dispatch；
- cleanup outcome 不被普通 return 覆盖；
- emitter 没有查询 raw IR 来重新决定 semantic family；
- archive schema 与 runtime capability 匹配。

## 9. 与 `walk`、逃逸分析和 SSA 的关系

### 9.1 不能丢失高层语义

`walk` 会把 channel send/receive、select 和许多 built-in 展开为 runtime 调用。
推荐两种实现方式，优先采用第一种：

1. 在 Unified IR 增加少量 compiler-internal coroutine semantic node，或给现有节点
   附稳定 SiteID；`walk` 读取 SitePlan 并保留对应的 `Await/Park/...`。
2. 将其展开为严格类型化的 internal intrinsic，但 intrinsic 必须携带 SiteID 和
   recipe ID，不能由后端按函数名识别。

`walk` 仍负责 Go 表达式求值次序、临时变量和普通 runtime lowering；coroutine 扩展
只接管计划内的 control cut。

### 9.2 逃逸分析必须 coroutine-aware

跨 suspend 数据至少分三类：

1. 只在同一 coroutine 中跨 suspend 存活的 SSA/local，进入 LLVM frame，不必因此
   自动成为普通 Go heap escape。
2. scheduler 在 park 期间访问的状态，进入稳定 `G` 或 operation record。
3. foreign/host producer 保留的 Go object，显式 root，并按 contract pin 或 copy，
   直到 terminal acknowledgement。

因此 final coloring 必须早于 `escape.Funcs`。逃逸分析需要理解 coroutine frame 是
一种独立 storage class；若简单把所有跨 suspend local 当 native stack local，会在
返回 executor 后悬空；若全部当 Go heap escape，虽然较安全但会显著增加分配和 GC
压力。

初期可以采用保守 heap/frame allocation，但测试和报告必须区分：

- Go heap escape；
- coroutine frame field；
- operation-owned retained root；
- pinned/copy foreign buffer。

### 9.3 SSA 切分点

Go 官方 SSA pass 列表在机器无关优化后进入 `lower`、`late lower`、schedule、
flag allocation 和 register allocation。LLVM 路径应：

1. 由 `ssagen` 构造包含 coroutine semantic op 的 generic SSA。
2. 运行适用的机器无关优化。
3. 在 target-specific `lower` 前停止官方 native pipeline。
4. 交给新的 `cmd/compile/internal/llvmgen`。
5. 生成 LLVM coroutine intrinsics，执行 LLVM verify 和 CoroSplit。

不能让普通优化跨越 suspend 随意搬动有副作用操作。新的 SSA op 必须正确声明 memory、
control、panic 和 safepoint 属性，必要时在优化前后运行 coroutine verifier。

### 9.4 不建议直接复用最终机器 SSA

最终机器 SSA 已包含架构寄存器、flags、ABI spill、stack slot 和平台指令，不再是合适
的可移植输入。把它翻译回 LLVM IR 会同时维护 Go 后端和 LLVM 两套 instruction
selection，并丢失 LLVM 优化空间。

## 10. LLVM ABI 与 frame

概念上的 coroutine primary 可以采用：

```text
CoroHandle F$coro(G*, ResultSlot*, Context?, args...)
```

具体 layout 需要单独 ABI 文档，至少冻结：

- handle、promise/header 和 result slot 布局；
- child-parent completion record；
- initial/final suspend；
- resume、done、destroy entry；
- panic/Goexit/abort/shutdown outcome；
- descriptor slot 和 root adapter；
- target pointer width、alignment 和 address space。

LLVM CoroSplit 负责把跨 suspend SSA value 放入 frame，但不能独自决定 Go GC root
metadata。CoroSplit 后还需要 LLGo pass：

1. 确认最终 frame layout。
2. 生成 frame descriptor 和 pointer bitmap/trace function。
3. 把 logical source positions 映射到 resume state。
4. 注册 frame owner 与 destroy path。
5. 验证任何 producer 都不持有裸 frame/LLVM handle。

原型阶段可以继续使用 LLGo 当前 GC、nogc 或保守 frame scan。若目标是兼容官方 Go
precise GC，则必须在 CoroSplit 后生成准确 root map，并重新审查 write barrier、
uintptr、unsafe、cgo pointer 和 finalizer 规则；这不是自动染色本身能解决的问题。

## 11. Go 控制语义

### 11.1 defer、panic、recover、Goexit

stackless coroutine 不能依靠保留的 native Go stack 或跨 frame `longjmp` 实现这些
语义。每个可能 cleanup 的 frame 需要显式状态：

```text
Running
  -> Draining(defer cursor, control stack)
  -> AwaitingDeferredCall
  -> PublishingCompletion
  -> FinalSuspended
```

必须保证：

- defer 参数在注册位置求值一次；
- LIFO cursor 在 deferred call 自身挂起后不重放；
- panic overlay、recover token、Goexit 和 task stop 相互区分；
- child 在销毁前把 outcome 发布到 parent-owned completion record；
- parent 只在 `Return` outcome 下读取普通 result；
- root boundary 在 destroy 前保存最终 outcome。

第一阶段可以禁止 suspendable defer/panic 路径，但不能用普通 native unwind 假装已经
支持。

### 11.2 channel 和 select

自动染色只能判定“这里可能 park”，不能替代 Go channel 语义。select 仍需保证：

- channel 和 send value 只求值一次；
- nil case disabled；
- default 只在没有通信可提交时选择；
- ready case 伪随机选择；
- closed receive 和 closed send panic；
- winner/loser 的原子 commit、cancel、detach；
- result lease 在 frame 恢复或 cleanup 时恰好消费一次。

因此 channel/select 应使用统一 `Park` family 和 typed recipe，而不是简单把
`runtime.selectgo` 标成 `MaySuspend`。

### 11.3 foreign、assembly 和系统栈

没有 Go body 的调用不能参加普通 SCC 推断，必须有 versioned callable contract：

- blocking/cost；
- thread/realm affinity；
- callback/reentry；
- retained pointer 和 result lifetime；
- cancel acknowledgement；
- worker/host/backend capability。

`systemstack`、nosplit、cgo callback、signal handler、assembly ABI 和 `LockOSThread`
均需显式策略。事实未知时拒绝或走保守 boundary，不允许在唯一 executor 上静默阻塞。

## 12. 官方 runtime 的兼容边界

官方 Go runtime 建立在以下条件上：

- 每个 G 有可增长和移动的 native Go stack；
- stack guard、morestack 和 async preemption；
- PC/SP stack map 和原生 traceback；
- open-coded defer、panic chain 和 system stack；
- 汇编调度器、signal、cgo 和 platform ABI。

stackless LLVM coroutine 会把 activation 移到 LLVM frame，并由 scheduler 显式
resume/destroy。二者不是替换一个函数调用就能兼容。

推荐分界：

- 首阶段复用 Go 官方 compiler frontend/middle-end。
- 使用 LLGo coroutine runtime、GC/frame contract 和 platform adapter。
- 逐步复用与 native stack 无关的纯 Go runtime/stdlib 代码。
- 把“运行未经修改的官方 runtime”列为独立长期研究，不作为自动染色原型的验收条件。

### 12.1 Push 与 pull-based 调度对比

原生 Go backend 的独立测量原型把两个维度分开：readiness 如何投递，以及 structured
call tree 如何表示。`Push` 把完成事件精确排入 logical task；同表示的 `Pull` 只唤醒
root，再从 root poll 到 ready leaf；两者使用完全相同的 task 和 typed frame。
`CompactPull` 另把 child task 融入 parent-linked frame，因此不是单纯的 pull 调度
结果。

同表示 `Push`/`Pull` 的结论不稳定。2026-08-14 的 native Darwin/arm64 聚焦结果曾让
pull 在 await depth 1–4096 快 1.06%–7.72%，但 merge 到最新 Go 开发检查点后的
2026-08-15 结果反转：

| Await depth | `Push` | 同表示 `Pull` | Pull 变化 |
| ---: | ---: | ---: | ---: |
| 64 | 7.001 us | 7.873 us | +12.46% |
| 256 | 24.03 us | 28.70 us | +19.44% |
| 4,096 | 441.9 us | 510.1 us | +15.43% |

两个模型在每个深度的 bytes/allocs 完全相同；entry、yield、timer、ready/blocking file、
ready/blocking socket 和 direct C 也没有稳定差异。这说明“root wake-and-poll”本身尚未
给出足够稳定的收益，不能据此增加第二套默认公开执行模型。

`CompactPull` 的大幅改善来自表示变化。在同一 post-sync run 的 depth 4,096，它把
441.9 us 降到 92.19 us（-79.14%），315,616 B/7,938 allocs 降到
131,312 B/4,098 allocs（-58.40%/-48.37%），GC-scanned heap 从 395,072 B 降到
198,488 B（-49.76%）。生产 push 原型随后把这个结论实现为 compiler-proven
structured handoff/frame fusion，而不引入 pull mode：异构互递归 depth 4,096 从
404.6 us 降到 298.6 us（-26.21%），allocation count 降 20.07%。

因此当前设计选择是 hybrid push：独立 goroutine root、external event、timer/I/O 和
blocking-C M handoff 继续使用 push；编译器证明 single-owner structured child 时直接
handoff 并融合 frame。Pull 继续作为测量模型，或未来显式 stream/backpressure API 的
候选；它不改变阻塞 C 调用必须把原 M 留在阻塞点、另给 ready work 提供 M 的要求。

## 13. 代码结构建议

在 Go compiler fork 中建议按职责分割：

```text
cmd/compile/internal/coro/
  summary.go       跨包 schema
  seed.go          高层 IR seed 和 callable fact
  effect.go        SCC fixed point
  funcrep.go       函数值流
  demand.go        primary/descriptor/adapter demand
  siteplan.go      稀疏调用点计划
  program.go       ProgramModelBuilder 与冻结
  verify.go        计划 verifier

cmd/compile/internal/noder/
  coro export/import extension

cmd/compile/internal/walk/
  保留和展开计划内 semantic node

cmd/compile/internal/ssagen/
  CoroProgramIR -> generic SSA

cmd/compile/internal/ssa/
  target-neutral Await/Park/Spawn/Poll/Cleanup ops

cmd/compile/internal/llvmgen/
  generic SSA -> LLVM IR
  coroutine emitter 与 CoroSplit 后处理

cmd/link/internal/ld/
  archive/runtime ABI digest 校验
```

LLGo runtime 继续按 scheduler、executor、operation source、platform adapter、GC/frame
metadata 分层。compiler package 不直接知道 timer、fd 或某个 syscall number。

`ir.Func` 可以持有不可变 `CoroSummaryID`/`FunctionPlanID`，但不建议把所有分析状态都
堆进 `ir.Func`。事实主体由 `coro.Program` 所有，避免各阶段分别修改同一组可变字段。

### 13.1 双模式 toolchain 与 coroutine 开关

开关应接入 Go 已有的 toolchain experiment 机制，建议名称为：

```text
GOEXPERIMENT=coro
GOEXPERIMENT=nocoro
```

在开发分支的 `internal/goexperiment.Flags` 增加 `Coro bool`。compiler/linker tool
使用 `buildcfg.Experiment.Coro` 或 `objabi.Experiment` 读取本次构建配置；
runtime/stdlib 使用生成的 `internal/goexperiment.Coro` 常量或
`goexperiment.coro` build tag。不应使用 `-d` 调试参数或只被 LLGo wrapper 理解的
环境变量作为影响 ABI 的主开关。

采用 `GOEXPERIMENT` 可以直接复用现有 Go 体系：

- `cmd/go` 将 canonical experiment string 放入 build action hash，切换后自动重编
  package graph；
- 产生 `goexperiment.coro` build tag，runtime/stdlib 可以选择受控的替代实现；
- experiment 会进入 toolchain/binary version；
- compiler、assembler、linker 和 runtime 可以看到同一配置；
- `go env GOEXPERIMENT`、script test 和交叉编译沿用现有交互。

开关选择的是一整套 toolchain strategy，而不是一个局部优化：

```text
GOEXPERIMENT=nocoro（默认）
  -> official frontend/middle-end
  -> official walk/generic SSA
  -> official arch lower/regalloc
  -> official linker/runtime/ABI

GOEXPERIMENT=coro
  -> official frontend/middle-end
  -> provisional/final coro analysis
  -> coroutine-aware walk/escape/generic SSA
  -> LLVM emitter/CoroSplit
  -> coroutine linker/runtime/ABI
```

两条路径共享语法、类型、Unified IR、普通优化和能够证明等价的标准库代码，但不在某个
函数中途动态切换 backend。`coro` 是 whole-toolchain、whole-program experiment；
所有 Go package、runtime 和 ABI-visible wrapper 必须使用一致的 experiment digest。

兼容性分为三层，不能混为一个承诺：

| 层次 | `nocoro` | `coro` |
| --- | --- | --- |
| Go 源码和语言语义 | 与 upstream 相同 | 目标为相同，按能力矩阵逐步认证 |
| `go build/test`、module、cache | 与 upstream 相同 | 复用相同工作流和 experiment cache key |
| object/archive/runtime ABI | 官方 ABI | coroutine ABI；初期不与官方 archive 混用 |

`coro` archive 至少携带 experiment、summary schema、coroutine ABI、runtime ABI 和 target
digest。linker 遇到缺失或不匹配的 Go archive 必须报错；不能因为函数当前看似
`NoSuspend` 就默认允许混合，因为函数值、runtime metadata、GC 和调用方假设仍可能
不同。C object、系统库和明确定义 ABI 的 foreign object 由 callable contract 单独
处理，不属于 Go archive 混用。

关闭模式必须是架构上的第一等路径：

- `coro` 未启用时不构建 Program/SitePlan，不写 coroutine private summary；
- inliner、escape、walk、SSA 和 linker 使用官方默认 strategy；
- native hot path 不为每条 IR/SSA instruction 支付动态接口调用；
- coroutine runtime 文件通过 experiment build tag 排除；
- 默认模式新增开销目标为零或可测量地接近零。

每次 upstream merge 和每个 LLGo 功能提交先过 native compatibility gate，再过
coroutine gate：

```text
native gate
  - make.bash/all.bash 与 upstream compiler tests
  - GOROOT stdlib/test
  - representative object/assembly/diagnostic differential
  - compiler time/memory regression

coroutine gate
  - experiment/cache/archive mismatch tests
  - analysis and ABI golden
  - LLVM verify/CoroSplit verify
  - runtime/stdlib/platform matrix
```

object comparison要排除 build ID、toolchain version 等预期差异；剩余差异必须有
allowlist。默认路径如果出现未解释的 codegen、诊断或性能差异，应视为 merge blocker，
不能用“coro 模式测试通过”覆盖。

### 13.2 长期开发分支与 upstream merge 策略

本方案维护一个基于 Go 官方开发线的长期分支，不维护一组在不同 Go 版本上反复重放的
patch，也不在代码中保留 `if go1.x`、重复 IR visitor 或多套 SSA adapter。

Go 官方并不是“master 上依次打 release tag”的单线模型。根据
[Go Release Cycle](https://go.dev/wiki/Go-Release-Cycle)、
[Minor Releases](https://go.dev/wiki/MinorReleases) 和本次对官方 Git refs 的检查：

- `dev.regabi`、`dev.unified`、`dev.simd` 等 `dev.*` 分支用于长期、大型开发工作。
  它们定期 merge `master`；成熟后再把整条分支或可独立部分 reverse-merge 到
  `master`。例如 `dev.regabi` 在 2021 年由 `84825599dc` merge 进入 master，
  `dev.unified` 在 2022 年由 `a10afb15e0` reverse-merge，当前 `dev.simd` 仍反复执行
  master merge 和分阶段 reverse-merge。
- `master` 是下一版本的唯一主要开发线。
- 每半年有一次主版本；开发期后进入 freeze/RC，随后 `master` 会在最终版本发布前重新
  开放下一版本开发，形成约一个月的重叠。
- 当前版本使用独立的 `release-branch.go1.N` 稳定；RC、正式版和 patch release tag
  都位于这条 release branch 上。
- release branch 在早期稳定阶段可 merge `master`；当 `master` 重新开放下一版本后，
  当前版本修复改为从 `master` 单独 backport/cherry-pick。
- 正式发布后，只有严重 bug、安全问题和少量安全文档/测试修改进入 release branch。
- Go 官方支持最近两个主版本，直到两个更新主版本已经发布。

因此 coroutine 主开发线最接近官方的 `dev.*` 模型，而不是 release branch 模型。
本 fork 保持 `cpunion/go:main` 与 Go 官方 `master` 一致，使用
`cpunion/go:dev.coro` 作为长期 coroutine 集成线；`upgrade/go-master` 和功能分支只
作为可审查的 topic branch，通过 PR 合入 `dev.coro`。若未来 Go 官方接受其中的通用
机制，可以按可独立审查的模块逐步贡献。下文直接使用 `dev.coro` 指这条长期开发线。

截至 2026-07-26 的本地 refs 直接证明了这项分叉：

- `origin/master` 为 `6db72bb92b`，已经在 Go 1.27 RC 之后继续开发。
- `origin/release-branch.go1.27` 为 `b7cc93a369`，`go1.27rc2` 为
  `075e9d41dc`，二者都不是 `master` 的祖先。
- 同一个 `cmd/compile` 修复在 master 是 `b590fd1075`，在 Go 1.27 release branch
  是独立的 `[release-branch.go1.27]` commit `b7cc93a369`。
- `go1.26.0`、`go1.26.5` 等 tag 同样只位于 `release-branch.go1.26` 的历史上。

因此必须修正一个策略：**不能把 release branch 或 release tag merge 进持续跟踪
master 的 `dev.coro`**。这会把 release-only backport 和可能已经以不同 commit
存在于 master 的修复重新引入开发线，造成重复历史和冲突。

推荐镜像 Go 官方的分支拓扑：

```text
go/master          A------B------C-----------D  (next Go development)
                           \
go/release.go1.N            R1---R2[tag rc]---R3[tag go1.N.0]---R4

dev.coro           A'--M(B)--Coro-------------M(D)---Coro
                            \
coro-release.go1.N           CR---M(R2)---------M(R3)------------M(R4)
```

- `dev.coro` 只 merge `go/master`，是 coroutine 新功能和未来上游贡献的开发线。
- Go 官方创建 `release-branch.go1.N` 时，从仍对应该版本的绿色 `dev.coro` 历史点
  创建 `coro-release.go1.N`。
- `coro-release.go1.N` 只 merge 对应的官方 release branch，不再 merge
  `go/master`，也不接受新的 coroutine 功能。
- 官方 tag 是不可变的认证锚点，不是升级源。对应 release branch 已经 merge 到
  `coro-release.go1.N` 后，不再额外 merge tag；只在通过测试的 coro commit 上打
  `llgo-go1.N...-coro` tag，并记录对应 upstream tag/hash。
- 已共享的 coro 分支不 rebase；整个 coroutine patch stack不会在每个 Go 版本重放。

如果官方切 release branch 时没有及时建立 coro release branch，并且
`dev.coro` 已经包含下一版本代码，应从历史上最后一个对应 release 的绿色 coro
commit 切分支。不能把 release tag 直接 merge 回最新 `dev.coro` 来“还原”版本。

开发线的日常升级仍使用临时 integration branch：

```text
从绿色 dev.coro 创建 upgrade/go-master
  -> git merge golang/go/master 的选定 commit
  -> 解决文本冲突
  -> 提交明确的 phase/API 适配
  -> 对比 phase-order 与 ABI manifest
  -> 运行 analysis、LLVM、runtime、stdlib 全矩阵
  -> merge upgrade branch 回绿色 dev.coro
```

这样保留官方 ancestry，`git log --first-parent` 能分别看到 Go 升级和 LLGo 功能，
`git bisect` 也不需要先重建一套 patch stack。解决冲突时禁止把旧版官方文件整体覆盖
新版文件；必须重新审查 hook 所在 phase 的输入、输出和时序。

稳定分支维护无法完全消除 backport，这是 Go 官方分叉模型本身决定的。正确的限制是：

- 不重放完整 coroutine patch stack；
- 新功能只进 `dev.coro`；
- 同时影响稳定版的 LLGo bugfix 以单个 commit 为单位 backport；
- 不把整个 `coro-release.go1.N` merge 回 `dev.coro`，否则会带回 upstream 的
  release-only commits；
- 安全修复同时跟随官方 master 和仍受支持的 release branch。

若项目只交付开发版，可以只维护 `dev.coro`，把官方 RC/tag 用作外部兼容测试基线。
若交付一个稳定版本，最低需要 `dev.coro + 一个 coro-release`。若承诺与 Go 官方
相同的“两代主版本”支持政策，就必须接受 `dev.coro + 两个 coro-release` 的维护
成本；一条分支无法同时精确表示三个已经分叉的提交图。

### 13.3 模块边界与最小官方 patch surface

建议把修改分为三层：

1. **官方文件中的最小 hook patch**

   只修改 experiment/backend 选择、phase registration、private export extension、
   inliner gate、escape storage class、walk/SSA dispatch 和 linker validation。每个
   hook 对应一个稳定 compiler phase，默认 native compiler 路径不改变语义。

2. **新增的独立 compiler 模块**

   `cmd/compile/internal/coro` 和 `cmd/compile/internal/llvmgen` 以新增文件为主。因为 Go
   的 `internal` 可见性限制，这些 package 位于 Go source tree 内，但不能把 LLVM、
   LLGo scheduler 或平台 runtime 细节反向暴露给 `ir`、`noder`、`walk` 和 generic
   SSA。

3. **LLGo backend/runtime**

   LLVM builder、scheduler、operation source、platform adapter 和 frame metadata
   继续由 LLGo 维护，通过冻结 ABI 与 compiler 模块对接。它们不应随 Go 升级复制进
   新的 compatibility directory。

依赖方向必须保持：

```text
Go Unified IR / generic SSA
        |
        v
target-neutral coro model and hooks
        |
        +------> official native strategy（默认、行为不变）
        |
        +------> LLGo LLVM strategy
                    |
                    v
             LLGo runtime ABI
```

不能让 generic compiler package import LLVM binding 或 LLGo runtime；也不应在几十个
官方文件中散布 `if llgoCoro`。策略差异集中在少量接口和 plan projection 中：

| 维度 | 官方 native 策略 | LLGo coroutine 策略 |
| --- | --- | --- |
| activation | 可增长 native G stack | stackless LLVM frame |
| preemption | stack guard/signal/resched | planned `Poll` + suspend |
| call ABI | Go ABIInternal/ABI0 | plain 或 coroutine primary |
| dynamic function | code/context ABI | FuncRep descriptor/dispatch |
| escape storage | stack/heap | stack/heap/coroutine frame/operation |
| unwind | 官方 panic/defer runtime | 显式 cleanup/outcome |
| backend | arch lower + regalloc | pre-lower LLVM emitter |

开发阶段可用独立 `GOEXPERIMENT` 或等价 toolchain experiment 选择策略，但 experiment
只负责选择，不承载版本兼容逻辑。

初始集成提交可以按以下职责拆分，但这些提交保留在长期分支中，不在升级时重放：

```text
toolchain identity and experiment/backend selector
private CoroSummary export/import
report-only effect analysis
provisional inliner gate
final Program/SitePlan freeze
coroutine frame storage in escape/liveness
semantic sites through walk
target-neutral coroutine SSA ops
pre-lower LLVM backend handoff
archive/runtime ABI validation
```

之后的 channel、timer、panic、reflect 或平台能力只增加 plan recipe、LLVM emitter
模板或 runtime 实现，不能继续扩大官方 phase hook。

可以把以下数字作为 merge conflict surface 的 review budget：

- 修改官方已有文件约 15–30 个，净改动尽量控制在 1,500–4,000 行；
- 新增 `internal/coro`、`internal/llvmgen` 等独立 compiler 代码约
  25,000–45,000 行；
- 每个被修改的官方文件都记录 hook purpose、phase invariant 和 upstream owner；
- CI 统计相对最近 upstream merge 的“已有官方文件改动清单”，意外扩大时失败；
- 测试可以大于实现代码，不能为了缩小 patch surface 删除 verifier 和 negative test。

### 13.4 面向 Go 官方贡献的切片

长期贡献到 Go 官方时，不应一次提交“LLGo LLVM backend”。建议按可独立审查的层次
推进：

1. 先贡献与 LLVM 无关的 bugfix、phase API 整理和测试能力。
2. 再讨论 compiler-private effect summary、显式 backend handoff、continuation
   storage class 等通用机制；每项先有独立 proposal 和官方 native consumer/test。
3. target-neutral effect、CoroProgramIR 和 verifier 可以作为实验功能评审。
4. LLVM emitter、LLGo function descriptor、scheduler/runtime ABI 和平台 adapter
   保持下游模块，除非 Go 项目明确接受对应 backend/runtime 方向。

为此，LLGo 提交应从开始就满足：

- generic semantic commit 不引用 LLGo symbol name、runtime function 或 LLVM type；
- policy 与 mechanism 分离，官方 native policy 始终有完整测试；
- schema/ABI 变化单独提交，功能实现不偷偷修改协议；
- 一个提交尽量只修改一个 compiler phase；
- downstream-only 文件有明确 ownership 标记，上游候选代码不依赖它们；
- 所有行为差异能在 strategy matrix 和 verifier 中枚举，不能靠隐式 fallback。

这会增加第一版的接口设计和双路径测试工作，但能避免完成后再从大 fork 中拆出可上游
部分。

### 13.5 与当前 `cpunion/llgo:llvm-coro` 的规模对比

以下统计只用于识别工作分布，不能把代码行直接换算为工期。以
`xgo-dev/llgo@56d8423700` 到
`cpunion/llgo:llvm-coro@9a0de9cb0` 为比较区间，当前分支包含：

- 917 个变化文件，约 244,112 行增加、5,978 行删除；
- 约 122,246 行 production、115,288 行 test、6,578 行设计文档增加；
- 364 个提交，其中 316 个非 merge 提交；
- `cl` 旧前端/lowering 约 46,233 行 production、41,256 行 test；
- `internal/coro` 分析约 14,871 行 production、12,573 行 test；
- LLVM coroutine builder 约 4,013 行 production、4,878 行 test；
- coroutine runtime core 约 25,357 行 production、22,337 行 test；
- runtime integration 约 11,655 行 production、5,545 行 test。

这些数字包含大量防御性验证、平台实现、测试矩阵和演进中的重复解释。基于官方
compiler 的主要收益不是“少写一个 parser”，而是删除 `cl` 中为 `x/tools/go/ssa`
重复维护的语言前端、wrapper、调用图和 lowering 预测。另一方面，官方 compiler
没有 LLVM backend，generic SSA 到 LLVM、coroutine ABI、runtime 和 GC 仍然需要实现。

预计可复用程度：

| 当前资产 | 直接代码复用 | 设计/测试复用 | 说明 |
| --- | --- | --- | --- |
| 四份 coroutine 契约 | 90% 以上 | 90% 以上 | 只需映射到官方 IR identity |
| scheduler/operation runtime core | 70%–85% | 80%–90% | 主要改 compiler ABI 和 frame 接口 |
| LLVM CoroBuilder | 60%–80% | 70%–90% | 保留物理 primitive，重接官方 SSA emitter |
| `internal/coro` 固定点 | 30%–50% | 70%–85% | 算法可保留，`x/tools/ssa` visitor 需重写 |
| `cl` coroutine lowering | 10%–25% | 50%–70% | 语义和失败用例可复用，IR 操作代码大多重写 |
| `internal/build` 集成 | 10%–30% | 50%–70% | 改接 `go build` toolchain/archive 流程 |
| runtime/stdlib patch | 50%–75% | 70%–90% | 取决于是否保持现有 LLGo runtime ABI |

下表以只维护 `dev.coro`、复用当前 runtime、由熟悉 Go
compiler/LLVM/runtime 的工程师实施，并从第一天保持上述模块和上游边界为前提：

| 累计目标 | 范围 | 预计工程量 |
| --- | --- | --- |
| Phase 0 | report-only 染色、跨包 summary、merge/hook 骨架 | 5–8 工程师周 |
| 可运行 MVP | plain/direct coro、一个 Park、单 native target、保守 GC | 14–22 工程师周 |
| 当前 `llvm-coro` 实验能力的同级实现 | channel/select、timer/worker、抢占、部分 cleanup/dynamic、多 target capability | 36–60 工程师周 |
| 可维护的生产级目标 | precise GC、完整 panic/defer/reflect、debug、stdlib、多 P 和平台认证 | 80–128 工程师周 |

这里的“当前同级”只表示达到当前分支已经演示和定向测试的能力，也保留其明确列出的
未完成项；不等于完整 Go 兼容。生产级目标已经超出当前 `llvm-coro` 分支的实际完成
范围。

相对于在现有架构中从零重做同样能力，官方 compiler + 长期 merge 分支预计把达到当前
实验能力的总工程量降低约 25%–40%，长期 Go 语义适配和升级维护量降低约 45%–65%。
但第一个可运行版本未必更快：必须先支付 generic SSA 到 LLVM backend、Unified IR
summary、escape/frame 集成以及可上游模块边界的成本。收益主要出现在功能继续扩大和
后续 Go 版本升级时。

在 patch surface 保持预算内时，一次常规 `go/master` merge 与认证预计为 3–8
工程师日；涉及 Unified IR、inliner、escape、SSA phase 或 ABI 的重大变化预计为
2–6 工程师周。这个数字是升级维护量，不包含随新 Go 版本增加语言特性、runtime
语义或平台能力的实现量。

稳定发布策略会增加独立维护量：

- 只维护 `dev.coro`：没有 release backport 成本，适合早期实验。
- 增加一个当前 `coro-release`：预计增加约 10%–20% 的持续维护和认证工作。
- 与 Go 官方一样维护两个稳定主版本：预计增加约 20%–35% 的持续维护和认证工作。

这里增加的主要是 upstream release merge、LLGo bugfix backport、双模式 stdlib/runtime
矩阵和安全发布，而不是复制 coroutine 实现。release tag 本身只用于冻结和认证，不应
计作一次代码 merge。

多人并行时，compiler analysis、LLVM backend、runtime 和测试可以分组推进，但
FunctionID、summary schema、primary ABI 和 frame/operation lifetime 必须先冻结。
由于这些公共契约形成串行关键路径，不能按人数线性缩短上述工期。

## 14. 实施阶段

### Phase 0：只分析、不改 ABI

- 增加默认关闭的 `GOEXPERIMENT=coro`，证明 `nocoro` 仍走完整官方路径。
- 增加 `cmd/compile/internal/coro`。
- 从 Unified IR 识别 direct call、go、defer、channel/select、loop 和动态调用。
- 用官方 SCC 求 effect fixed point。
- 实现 compiler-private summary export/import round trip。
- 输出稳定 JSON/text dump、schema digest 和诊断。
- 与 `llvm-coro` 当前分析结果在选定 corpus 上对照。

验收：upstream native gate 通过且没有未解释的输出/性能差异；增量编译、跨包、递归
SCC、go/defer 差异和 summary mismatch 测试通过；重复构建 digest 一致。

### Phase 1：闭合 direct-call vertical slice

- 只支持一个显式 `Park` seed。
- 禁止动态函数值、defer/panic、reflect、cgo、assembly。
- `NoSuspend` 走 plain primary。
- `MaySuspend` 走 coroutine primary。
- direct managed call 自动生成 structured await。
- 新 LLVM emitter 在 `lower` 前接管 generic SSA。

验收：三层以上调用链自动染色；每个源码函数只有一个 primary；LLVM verify 和
CoroSplit 后 verify 通过。

### Phase 2：抢占、channel/select 和 operation

- 生成 `NeedsPreempt` 与 `Poll`。
- 接入 channel/select typed Park recipe。
- 接入 timer、worker、foreign 和 host operation。
- 建立 stable G、OperationID、result lease、cancel/detach。

验收：循环公平性、early completion、cancel race、select winner/loser、无 executor
阻塞和跨平台 capability 诊断通过。

### Phase 3：显式 cleanup/outcome

- defer registration 和 suspendable deferred call。
- panic/recover/Goexit/abort/shutdown state machine。
- parent-owned completion record。
- root boundary outcome。

验收：与 Go 官方语义做 differential test，覆盖 nested defer、panic replacement、
recover、Goexit 和 cancel 竞态。

### Phase 4：动态函数值与语言元数据

- FuncRep value-flow。
- descriptor、interface itab、method value。
- generic wrapper、reflect Call/MakeFunc。
- callback 和 hard-sync adapter。

验收：direct path 不被无谓 descriptor 化；开放动态调用不可能从 plain activation
进入 coroutine；ABI mismatch fail closed。

### Phase 5：GC、debug 与工具链

- CoroSplit 后 frame root map。
- write barrier、unsafe/cgo pointer 规则。
- logical stack trace、pclntab/DWARF 和 debugger state。
- coverage、race、asan/msan 可支持范围。

验收：GC stress、frame destroy、logical traceback、source line、coverage 和 debugger
矩阵通过。

### Phase 6：标准库和平台

- native、WASI、browser/JSPI、embedded/RTOS adapter。
- 标准库 blocking API catalog。
- cgo、assembly、signal 和 affinity。
- 构建缓存、link、plugin/shared mode 策略。

验收：按平台声明 capability；不支持的组合在编译或链接期明确失败，不静默降级。

## 15. 测试矩阵

每个阶段至少覆盖：

| 类别 | 关键用例 |
| --- | --- |
| 固定点 | 深调用链、直接递归、互递归、imported summary |
| 边类型 | direct、defer、go、interface、closure、reflect |
| 求值 | 参数、副作用、select operand 恰好执行一次 |
| 抢占 | 空循环、递归、unknown-cost call、公平性 |
| 生命周期 | early wake、cancel、destroy、stale generation |
| cleanup | return、panic、recover、Goexit、defer 再挂起 |
| ABI | 缺 summary、旧 schema、普通 gc archive、错误 target |
| GC | pointer 跨 suspend、retained foreign buffer、frame destroy |
| debug | logical stack、resume source line、panic traceback |
| 平台 | native、WASI、browser、embedded capability |

测试层次应包括：

1. analysis golden：函数 effect、call plan、FuncRep 和 digest。
2. verifier negative：故意构造错误计划，确认 fail closed。
3. LLVM IR structural：coro intrinsic、resume/destroy 和单 primary。
4. runtime stress：调度、operation、取消、GC。
5. Go differential：同一程序与官方 Go 的输出、panic 和求值次数比较。
6. GOROOT/标准库矩阵：按已声明能力逐步扩大，不用“可以编译”代替“语义通过”。

## 16. 主要风险与决策

| 风险 | 影响 | 建议 |
| --- | --- | --- |
| 内联早于染色 | frame owner 和 ABI 错误 | provisional effect 先限制内联 |
| 跨包 summary 缺失 | caller 无法选择 ABI | 同 toolchain 重编或 fail closed |
| `walk` 丢失语义 | 按 runtime 名猜测、求值重放 | SiteID + semantic node/typed intrinsic |
| 函数值与 effect 混合 | body 复制或错误 dispatch | 独立 FuncRep fixed point |
| 官方 escape 不认识 frame | 悬空或过度 heap escape | final coloring 在 escape 前，增加 storage class |
| CoroSplit 后无 GC map | pointer 丢失 | 自定义 post-split frame metadata pass |
| native runtime 假设 | signal/stack/unwind 错误 | 初期使用 LLGo runtime |
| 动态调用过度保守 | 代码体积和调度开销 | 先正确，再逐步闭合 candidate set |
| API-specific opcode 膨胀 | compiler/runtime 强耦合 | 固定 semantic family + typed recipe |
| linker 才发现 effect | 已生成 caller 无法修正 | compile-time imported summary |
| 默认路径被 coroutine hook 影响 | 无法跟随和贡献 upstream | `nocoro` native gate 与 differential |
| 普通/coroutine archive 混用 | ABI、GC 或函数值错配 | experiment/ABI digest，link 时拒绝 |

## 17. 待验证问题

以下问题不能在架构稿中假定已经解决：

1. 官方 inliner 的哪些 cost、PGO 和 wrapper 路径需要 coroutine-aware 扩展。
2. generic SSA 中最小的新 op 集，以及哪些现有优化可以安全跨 suspend。
3. coroutine frame storage 如何与官方 escape、liveness 和 write barrier 精确对接。
4. interface/reflect 元数据是在现有 ABI 上加 shadow descriptor，还是定义新的 toolchain
   ABI。
5. LLVM CoroSplit 后 frame root map 在各 LLVM 版本和 address space 下的稳定性。
6. panic/defer/recover 的哪些官方 compiler 优化可以复用，哪些必须改为显式 state
   machine。
7. cgo、assembly、signal 和 system stack 在各 target 的可支持边界。
8. 是否需要受限 LTO 来缩小动态候选和 demand；LTO 只能优化，不能替代 producer
   summary 的正确性。

## 18. 建议的第一步

Phase 0 已完成；第 1.2 节的 standalone basic example 和第 1.4 节的受限
object/archive/link ownership 纵切也已实际运行。下一步仍不应扩大到 channel、panic、
GC 或标准库，而应先把以下三个 Phase 0 决定补齐成稳定 Program/SitePlan：

1. Unified IR 是否保留了当前 `llvm-coro` 分支分析所需的全部语义。
2. 两阶段染色是否能与官方内联、wrapper、逃逸顺序稳定共存。
3. 跨包 summary 能否进入 Unified IR private extension、build cache 和 linker 校验。

Phase 0 的产物应能对同一组程序同时打印：

```text
FunctionID
LocalSeed
ImportedEffect
FinalEffect/Exec
CallEdge(kind, target)
FuncRep
Demand
PrimaryKind
SummaryDigest
```

当它与 `llvm-coro` 现有规则在 direct/defer/go、递归、动态调用和抢占 corpus 上一致
后，再把已通过的单函数 backend ownership 扩成 typed Yield、reentry scheduler 和
三包 direct-call structured await。这样仍能把自动染色、包边界、LLVM lowering 和
runtime 生命周期分层验证。

## 19. 最小可行性验证范围

Phase 0 只能证明分析和跨包数据可接入，不能证明官方 compiler 到 LLVM coroutine 的
物理路径可行。建议用一个严格 timebox 的 executable vertical slice 作为最终
go/no-go 验证。

当前完成度应明确区分：

| 能力 | 当前状态 |
| --- | --- |
| 默认关闭 experiment、native compiler gate | 已验证 |
| 跨包 effect summary 与 mixed archive fail-closed | 已验证 |
| generic SSA 在 target lower 前 handoff | 已验证 |
| 单函数 scalar SSA 生成 standalone LLVM coroutine | 已验证 |
| initial/final suspend、resume、done、result、destroy | 已在 LLVM 19.1.7 执行验证 |
| typed `YieldOnce`/SiteID、三包 coroutine primary/await | 未实现 |
| 单函数 Go object/archive/linker ownership | 已在 Darwin/arm64 受限验证 |
| 单线程 push FIFO、early/late operation reentry | 已在 Darwin/arm64 受限验证 |
| native timer event、跨线程 publication、ready task progress | 已在 Darwin/arm64 与 Linux/amd64 受限验证 |
| blocking file read、replacement executor、queue ownership handoff | 已在 Darwin/arm64、Linux/arm64 和 Linux/amd64 聚焦用例受限验证 |
| connected stream socket、blocking recv/send、queue ownership handoff | 已在 Darwin/arm64、Linux/arm64 和 Linux/amd64 聚焦用例受限验证 |
| `time.Sleep` lowering、timer service、TCP/netpoll、普通 file lowering、三包 structured await | 未实现 |

因此 basic example、object/link 和手工 operation reentry 纵切已经回答第 19.1 节的
第 4、5 个物理路径问题，并验证了最小单线程 scheduler 所有权；但不能替代第 19.4
节的完整验收，也不能把手工 publisher 当成 timer/netpoll runtime 已接通。

### 19.1 必须回答的问题

最小验证只回答六个高风险问题：

1. `GOEXPERIMENT=coro` 关闭时能否保持完整官方路径。
2. imported summary 加本包 SCC 能否在内联、escape 和 walk 前完成自动染色。
3. 一条普通 Go direct-call chain 能否在不增加源码 `async/await` 的情况下传播
   `MaySuspend`。
4. 官方 generic SSA 能否在 arch `lower` 前交给 LLVM emitter。
5. LLVM coroutine 能否真实 suspend、resume、return 和 destroy，并保持 suspend 前
   副作用只执行一次。
6. 该集成能否在一次真实 upstream master merge 后仍只修改少量稳定 hook。

它不用于证明标准库、GC、panic、动态函数值或多平台已经可行。

### 19.2 固定实现范围

基线和构建：

- 在从最新 `go/master` 创建的 `dev.coro` worktree 中实施，记录精确起点；较大的
  PoC 步骤仍以短期 topic branch/独立 commit review，不再创建另一条长期开发线。
- 增加默认关闭的 `GOEXPERIMENT=coro`。
- 只运行一个 host target；第二 target、交叉运行和性能优化不进入本验证。
- `nocoro` 使用官方 compiler/linker/runtime。
- `coro` 使用当前 LLGo LLVM builder 和最小 scheduler/runtime slice；不要求先移植
  完整官方 runtime。

编译器：

- 只实现 `NoSuspend`、`MaySuspend` 两点 effect lattice。
- 只识别 direct managed call；不支持动态候选。
- 实现最小 `CoroSummary` export/import，只含 FunctionID、effect、primary ABI、
  schema version 和 digest。
- provisional pass 只负责禁止 `MaySuspend` 的普通内联。
- final pass 在 escape/walk 前冻结三包调用链的 FunctionPlan/SitePlan。
- 只增加 `Yield` 和 `Await` 两种 coroutine semantic op。
- LLVM emitter 只支持测试程序实际使用的 generic SSA 子集。

唯一 suspend seed：

- 使用 compiler-owned、版本化的 typed `YieldOnce` recipe。
- recipe 先只通过 GOROOT 内部测试 package 暴露，不增加公开 Go API。
- seed 由 private callable fact/SiteID 识别，不根据 link symbol 字符串猜测。
- runtime 行为仅为把当前 continuation 重新放入队列并恢复一次，不实现 timer、I/O 或
  cancellation。

固定测试程序由三个 package 组成：

```text
main/root
  -> mid
       -> leaf
            -> scalar side effect
            -> YieldOnce
            -> use scalar live across suspend
            -> return result
```

`leaf` 建立 local seed，`mid` 和 `root` 只能通过 imported summary 自动染色。程序记录
suspend 前后的计数和最终 scalar result，用于证明没有重放 source side effect。只允许
scalar 跨 suspend，不允许 Go pointer，以免在本阶段提前引入 GC root map。

构建和链接：

- 至少产生 package archive 和 private summary round trip，不只在内存中分析。
- `GOEXPERIMENT=coro` 必须进入 action/cache identity。
- root 使用一个最小 reentry adapter 驱动 scheduler 直到 completion。
- LLVM IR 在 CoroSplit 前后各运行一次 verifier。
- linker 对普通/coroutine archive 混用给出稳定诊断。
- 最小 runtime 可以作为显式版本化的 foreign/runtime boundary 预构建；本验证不声称
  已能用新 backend 编译完整 GOROOT runtime。

### 19.3 明确排除

以下任一项都不能在 spike 中顺手扩入：

- `go` statement、多 G 调度和抢占 poll；
- channel、select、timer、network、worker 和 host Promise；
- defer、panic、recover、Goexit 和 cancellation；
- closure、interface、reflect、generic dynamic dispatch 和 function descriptor；
- Go pointer 跨 suspend、precise GC、write barrier 和 finalizer；
- cgo、assembly、signal、system stack 和 thread affinity；
- DWARF、pclntab、logical traceback、coverage 和 debugger；
- 完整标准库、完整官方 runtime、WASI/browser/embedded；
- 多 target、性能调优和 release branch 发布。

发现上述需求时先记录 issue/后续任务；只有它实际阻断这条 scalar direct-call slice
时，才把阻断事实写入结论，而不是扩大 spike。

### 19.4 验收和停止条件

全部满足才判定“架构可继续”：

1. `nocoro` 的 `make.bash/all.bash`、compiler tests 和代表性 native differential
   通过。
2. analysis golden 显示 `leaf -> mid -> root` 的跨包 effect 正确，schema/digest
   重复构建稳定。
3. `coro` 程序真实执行一次 suspend/resume，最终 result 正确，suspend 前计数恰好为
   一。
4. 每个 source function 只有一个 primary body；plain primary 中不存在 suspend。
5. LLVM pre/post-CoroSplit verify 通过，frame 能在 completion 后 destroy。
6. cache 不复用另一 experiment 的 archive；mixed archive 稳定失败。
7. 在 spike 期间选择一个更新的 `go/master` commit 做一次真实 merge，重新通过上述
   gate；不得复制旧官方文件覆盖冲突。
8. 官方已有文件的修改目标不超过 10–15 个、1,500 行；超出时必须在报告中逐项说明。

出现以下任一项应暂停并重新评审设计：

- 需要在 20 个以上官方 package 中散布 coroutine 条件才能运行测试程序；
- imported effect 无法在 inliner 前取得，必须把所有 imported call 永久视为动态；
- 必须等到 arch-specific lower 后才能取得正确 Go 语义；
- scalar frame 也依赖完整 precise GC 或官方 native stack；
- `nocoro` 无法保持官方 phase order、诊断或 codegen；
- 一次普通 upstream merge 就要求重写核心分析或 emitter。

### 19.5 工期范围

以一名熟悉 Go compiler、LLVM 和当前 LLGo coroutine 代码的工程师计算：

| 工作项 | 工程师日 |
| --- | ---: |
| 分支、experiment 开关、native gate | 3–5 |
| provisional/final effect 与 summary round trip | 7–10 |
| generic SSA 子集、LLVM emitter、CoroSplit | 10–15 |
| root adapter、最小 yield scheduler、link | 5–8 |
| negative tests、upstream merge rehearsal、报告 | 5–8 |
| 合计 | 30–46 |

推荐承诺范围为 **6–10 工程师周，最多 10 周停止并出结论**。Phase 0 的分析、summary
或 native gate 如果在前 2–3 周已经暴露结构性阻断，应提前停止，不继续投入 LLVM
runtime。

两名有对应经验的工程师可以把日历时间压到约 4–6 周，但不能简单减半，因为 summary
schema、SSA handoff 和 primary ABI 是串行依赖。该范围不包含生产化，也不应据此下结论
“当前 `llvm-coro` 功能已经可以迁移”。
