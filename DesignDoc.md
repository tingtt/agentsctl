# DesignDoc: agentsctl

**Document Status:** Draft  
**Development Status:** TBD

## Abstract/Summary

agentsctl は、Claude Code のバックグラウンドエージェントと Codex CLI のセッションを、単一の Agent View から操作する Unix TUI である。

提供する共通操作は以下とする。

- List
- Dispatch
- Attach / Detach
- Stop
- Rename
- Archive
- Pin / Unpin

Claude と Codex では、バックグラウンド実行の仕組みが異なる。
そのため、以下の非対称な実行モデルを前提とする。

- **Claude**
  - ネイティブなバックグラウンド実行環境を利用する。
  - セッションの実行主体は Claude 側が保持する。
- **Codex**
  - agentsctl が supervisor と PTY を提供する。
  - supervisor が Codex CLI プロセスを保持する。

各 provider のネイティブなライフサイクルを維持しつつ、Agent View 上では共通の UX を提供する。

### Provider runtime model

| 項目                 | Claude                  | Codex                                               |
| -------------------- | ----------------------- | --------------------------------------------------- |
| バックグラウンド常駐 | Claude のネイティブ機構 | agentsctl supervisor + PTY                          |
| Session catalog      | `claude agents`         | Codex app-server + agentsctl managed run            |
| Dispatch             | `claude --bg`           | supervisor に Codex CLI の起動を依頼                |
| Attach               | `claude attach`         | supervisor が保持する PTY へ接続                    |
| Stop                 | `claude stop`           | supervisor が所有するプロセスを停止                 |
| Rename               | agentsctl-local overlay | Codex app-server                                    |
| Archive              | agentsctl-local overlay | Codex app-server、または unbound run のローカル削除 |
| Pin                  | agentsctl-local state   | agentsctl-local state                               |

## Background

Claude Code と Codex CLI は、どちらも継続可能なエージェントセッションを扱える一方で、バックグラウンド実行のライフサイクルやセッションへの再接続までの操作体験に大きな差がある。

agentsctl はこの provider ごとの差異を吸収し、複数セッションを一元的に管理する経路を提供する。

## Goals

- Claude と Codex のセッションを単一の Agent View から一覧・操作できるようにする。
- 以下を provider に依存しない共通操作として提供する。
  - list / dispatch / attach / detach / stop / rename / archive / pin

- Codex でも、TUI の終了後に同じ interactive process へ再接続できるようにする。

## Non-Goals

- Claude / Codex 以外の provider への対応
- Archive したセッションを Agent View から復帰させる操作
- agentsctl 独自の session / transcript 形式を持つこと
- Windows 対応

## Proposed Design

agentsctl は、Claude と Codex を共通の session model として Agent View へ提示する。ただし、実際の lifecycle operation は provider ごとの能力へ委譲する。

agentsctl が補うのは、主に Codex に不足するバックグラウンド実行能力である。

### UX Design

#### Session catalog

Claude と Codex のセッションを統合し、1つの一覧として表示する。

session は作成時刻が新しい順に並べる。Activity や runtime status の変化だけでは並び順を変更しない。これにより、バックグラウンド更新によって閲覧中の行が頻繁に移動することを防ぐ。

##### Pin / Unpin

Pin 状態は agentsctl が永続化する。

Pin / Unpin 操作は即時に表示へ反映するため、provider の catalog を再取得せず、現在の一覧へ ordering rule を再適用する。

#### Lifecycle

Agent View では Claude と Codex を共通の session model として扱うが、session の実体と実行主体は provider ごとに異なる。

**Claude**

- `claude agents` が提供する native session を session の実体とする。
- native lifecycle state を Agent View の共通 Activity へ変換する。
  - Working
  - Needs input
  - Waiting quota
  - Completed
  - Failed
- background session の実行主体と lifetime は Claude 側が保持する。
- agentsctl の TUI や attach client の lifetime とは独立して存在する。

**Codex**

- Codex app-server が提供する thread を session の実体とする。
- agentsctl が起動した interactive Codex CLI は managed run として別途追跡する。
- managed run が Codex thread と対応付いた後は、両者を1つの session として Agent View に提示する。
- thread とまだ対応付いていない managed run も、session catalog 上で状態を確認できる。
  - 起動中: `Starting`
  - thread に対応付かないまま終了: `Unbound run`

- interactive Codex CLI process と PTY の lifetime は agentsctl supervisor が保持する。
- TUI の lifetime と Codex CLI process の lifetime は分離する。

#### Dispatch / Composer

Composer に入力した prompt を、選択中の provider へバックグラウンド dispatch する。

provider ごとの起動方法は異なるが、Agent View 上では同じ Dispatch 操作として扱う。

**Claude**

- native background dispatch を利用する。
- 起動後の session lifetime は Claude 側へ委譲する。

**Codex**

- Dispatch 前の thread 一覧を記録する。
- agentsctl supervisor が以下を行う。
  1. PTY を作成する。
  2. PTY 上で Codex CLI を起動する。
  3. managed run として process を追跡する。

- TUI 自身は Codex CLI process を直接保持しない。
- 起動後に追加された Codex thread と managed run を対応付け、通常の session として catalog に統合する。

##### Prompt stash

Composer は、1つの共有 prompt stash を持つ。

stash の特徴:

- provider 間で共有する。
- directory scope 間で共有する。
- 選択 session には紐付かない。
- memory 上だけに保持する。
- agentsctl 終了時に破棄する。

Attach 中は terminal input を対象 CLI へ渡すため、Composer / stash の操作とは分離する。

#### Attach / Detach

Attach すると、Agent View から対象 CLI へ terminal を明け渡す。

Detach すると、session を停止せず Agent View へ戻る。

**Claude**

- `claude attach` を agentsctl が用意した PTY 上で起動する。
- Detach では、まず Claude attach client 自身の detach mechanism を利用する。
- attach client が終了しない場合のみ、agentsctl が所有する attach client の process group を終了する。
- Claude の native background session 自体には signal を送らない。

**Codex**

- managed session では、supervisor が保持する PTY へ Unix socket 越しに接続する。
- PTY の lifetime は attach client と独立させる。
- 以下の場合も Codex CLI の実行を継続する。
  - Detach
  - TUI の終了
  - TUI の再起動

##### External Codex thread

agentsctl がまだ管理していない Codex thread でも、writer が存在しないことを確認できれば Attach できる。

この場合は既存 process へ再接続するのではなく、以下の流れで managed run へ移行する。

1. 既存 thread を resume する。
2. 新しい managed run を起動する。
3. supervisor がその PTY を保持する。
4. Agent View からその PTY へ Attach する。

UI 上では通常の `Attach` として扱う。

#### Session actions

既存 session に対して、Stop / Archive / Rename を提供する。

これらは異なる状態へ作用する独立した操作とし、他の操作を暗黙に伴わない。

##### Stop

Stop は、session に紐づく実行中の process を終了する。

**Claude**

- native stop を利用する。
- agentsctl 側で独自 process lifecycle を持たない。

**Codex**

- supervisor が所有している managed process のみ停止する。
- ownership を証明できない writer は停止しない。

Stop は以下とは独立する。

- Detach
- Archive

##### Archive

Archive は、既存 session を通常の Agent View から除外する。

**Claude**

- agentsctl-local overlay とする。
- 以下には変更を加えない。
  - native session
  - transcript
  - worktree

**Codex thread**

- app-server の native archive を利用する。

**Codex Unbound run**

- Unbound run は Codex thread ではないため、native archive API には送らない。
- 以下を確認したうえで local run state から削除する。
  - terminal state である。
  - thread に未対応付けである。

実行中の session は Archive できない。

Archive が暗黙に Stop を実行することもない。

##### Rename

Rename は、既存 session の表示名を変更する。

**Claude**

- agentsctl-local overlay とする。
- session ID に対応する表示名を agentsctl が保持する。
- Claude 自身の以下には変更を加えない。
  - session state
  - transcript
  - worktree

**Codex**

- app-server の native rename を利用する。

#### Directory scope

Agent View は、agentsctl を起動した directory を基準に3つの scope を持つ。

| Scope    | 対象                                     |
| -------- | ---------------------------------------- |
| `cwd`    | 起動 directory と CWD が一致する session |
| `cwd/**` | 起動 directory 自体と、その descendant   |
| `all`    | 全 session                               |

scope は以下の順で切り替える。

```text
cwd → cwd/** → all → cwd
```

##### Path matching

`cwd/**` は path boundary を考慮する。

例えば以下の場合:

```text
/project
/project/src
/project-other
```

`/project` を基準とした `cwd/**` に含むのは以下。

```text
/project
/project/src
```

`/project-other` は含めない。

単純な文字列 prefix matching は使用しない。

##### Symlink

基準 directory は logical path として扱う。

symlink は解決しない。

##### Persistence

Directory scope は表示状態であり、永続化しない。

#### Feedback and confirmation

operation の結果が UI の変化から明確に分かる場合、追加 notification は表示しない。

例:

- session が追加される
- editor が閉じる
- pin によって行が移動する
- archive confirmation が消える

##### Global errors

Composer 上部の notification area は error 用とする。

例:

- 操作が拒否された
- provider が利用できない
- capability がない
- 入力値が不正

##### Row notice

特定 session にだけ関係する notice は、その session row に表示する。

Archive confirmation は row notice として扱う。

確認状態は session identity に紐付けるため、以下をまたいでも同じ session に追従する。

- Refresh
- Pin / Unpin
- Reordering

##### Width priority

terminal width が不足する場合、表示優先度を設ける。

優先する情報:

1. Provider
2. CWD
3. Row notice
4. Title

session の識別に必要な情報を優先し、補助情報から省略する。

### Implementation Design

#### Design principles

実装全体では、以下を基本原則とする。

1. **Native state is canonical**
   - provider が所有する session state を source of truth とする。
2. **Local state is supplemental**
   - agentsctl 固有の metadata のみ保持する。
3. **Provider differences stay explicit**
   - Claude と Codex の runtime model の差を無理に同一化しない。
4. **Process operations fail closed**
   - ownership や identity を証明できない process には介入しない。
5. **TUI lifetime and agent lifetime are separated**
   - TUI の終了が background agent の終了を意味しない。

#### Landscape

```mermaid
C4Container
title agentsctl system landscape

Person(user, "User", "Claude Code と Codex CLI のセッションを操作する")

System_Boundary(agentsctl, "agentsctl") {
    Container(tui, "Agent View", "Go / Unix TUI", "統合された session catalog と操作 UI")
    Container(catalog, "Session Model", "Go", "provider 固有状態を共通 session capability へ正規化する")
    Container(state, "Local State", "JSON / file locking", "pin、overlay、managed run metadata を保持する")
    Container(supervisor, "Codex Supervisor", "Go / Unix daemon / PTY", "Codex CLI process と PTY の寿命を管理する")
}

System_Ext(claude, "Claude Code", "Native background agent lifecycle")
System_Ext(codexAppServer, "Codex app-server", "Thread metadata and native thread operations")
System_Ext(codexCLI, "Codex CLI", "Interactive agent process")

Rel(user, tui, "操作")
Rel(tui, catalog, "list / dispatch / session actions")
Rel(catalog, claude, "native lifecycle operations")
Rel(catalog, codexAppServer, "thread list / rename / archive")
Rel(catalog, state, "local metadata")
Rel(tui, supervisor, "start / attach / stop")
Rel(supervisor, state, "managed run metadata")
Rel(supervisor, codexCLI, "PTY 上で起動・入出力")
```

この図は概念上の責務境界を示す。

Go package の構成そのものは規定しない。

#### Common session model

Agent View は provider 固有 object を直接扱わず、共通の session model へ正規化する。

主な情報は以下。

- Provider
- Native session identifier
- Display name
- Summary
- CWD
- Created time
- Updated time
- Activity
- Runtime ownership
- Archive state
- Pin state
- Capabilities

Capabilities には以下を含む。

- Attach
- Stop
- Rename
- Archive
- Unarchive
- Respawn

UI は provider 名だけで操作可否を判断せず、各 session が持つ capability を基準に action を提供する。

#### Native state and local overlays

provider の native state を canonical state とする。

agentsctl は native session record や transcript を複製せず、agentsctl 固有の metadata のみ local state に保持する。

##### Local state

主に以下を保持する。

- Pin
- Claude Archive overlay
- Claude Rename overlay
- Codex managed run metadata

##### Overlay

overlay は native result の取得後に適用する。

用途:

- provider に該当する native API がない場合
- native state を変更すべきでない場合
- agentsctl 固有の表示状態を持つ場合

overlay を native state の代替 source of truth としては扱わない。

##### Persistence

local state は複数 process から利用される。

そのため、更新時は以下を保証する。

- file lock
- read-modify-write の排他
- atomic な置き換え
- partial write を通常状態として公開しない

#### Codex supervisor

Codex CLI の interactive process と PTY は、Agent View の TUI process とは別の supervisor が所有する。

##### Responsibilities

supervisor は以下を担当する。

- managed run の起動
- PTY の保持
- Attach client との入出力
- terminal resize
- Detach
- managed process の Stop
- managed run metadata の更新

##### Lifetime

TUI が終了しても、supervisor と Codex CLI が生存していれば PTY は維持する。

supervisor が終了した場合、既存 PTY は復元できない。

その場合は process を推測して再利用せず、既存 managed run を stale として扱う。

#### Supervisor IPC

TUI と supervisor の通信には Unix socket を利用する。

socket 上では、length-prefixed frame protocol を使用する。

##### Control frames

- Request
- Response
- Exit
- Failure

##### PTY frames

- Input
- Output
- Resize
- Detach

control request / response と PTY stream を、同じ framing mechanism で扱う。

##### Compatibility

supervisor とは以下の compatibility を確認する。

- Protocol version
- Build generation

互換性を確認できない daemon を、そのまま再利用しない。

#### Process ownership and identity

PID 単独では process identity として扱わない。

PID は再利用される可能性があるため、以下を組み合わせる。

- PID
- Process start time
- UID

process に介入する直前に、OS から identity を再取得する。

記録値と一致しない場合は操作を拒否する。

##### Fail-closed cases

以下の場合も推測しない。

- process が見つからない
- identity を確認できない
- ownership が一致しない
- 複数 candidate が存在する

#### Codex run-to-thread binding

Codex の managed run は、起動直後には app-server thread ID を持たない。

そのため、Dispatch 前の thread 一覧を baseline として保持する。

起動後、以下から candidate を絞る。

- baseline に存在しなかった thread
- CWD
- writer ownership

candidate が1つの場合のみ binding する。

```text
managed run -> Codex thread
```

candidate が以下の場合は binding しない。

- 0件
- 2件以上

session ID を推測して割り当てることはしない。

#### PTY attach and redraw

Codex Attach では、過去の PTY output を replay しない。

新しい attach client は接続後の output のみ受け取るため、画面復元は Codex CLI 自身の redraw に依存する。

##### Same-size reattach

再 Attach 時に terminal size が前回と同じ場合でも、確実に redraw させる必要がある。

そのため、PTY size を以下の順で変更する。

1. 一時的な別 size
2. 実 terminal size

これにより actual resize event を発生させ、Codex 自身の redraw lifecycle を利用する。

この処理は表示だけに関与し、conversation state には関与しない。

#### Concurrency and backpressure

##### Catalog loading

Claude と Codex の session list は provider ごとに並行取得する。

目的は latency を加算させないこと。

```text
Claude: ──────────┐
                  ├─ merge
Codex:  ───────┐  │
               └──┘
```

1回の refresh は、provider ごとの取得完了を待ってから統合する。

partial result を逐次描画する方式にはしない。

provider が利用できない場合は、その error を provider 単位で保持する。

他方の provider から取得できた session は破棄しない。

##### PTY output

supervisor は PTY output を attach subscriber へ配信する。

subscriber が遅い場合でも、PTY 自体の read loop を停止させない。

session process の進行を UI client の描画速度に依存させない。

#### OS boundaries

以下には OS 固有機能を利用する。

- process start time の取得
- Unix socket peer identity の取得
- process group 操作
- PTY 操作

安全性要件を維持できない platform では、推測による fallback を実装しない。

MVP の対象 OS は以下。

- macOS
- Linux

## Alternatives Considered

### Codex CLI を TUI の子 process として保持する

**不採用。**

TUI の終了とともに interactive process への接続経路を失い、TUI 再起動後に同じ PTY へ戻れない。

そのため、Codex process と PTY の寿命を TUI から分離する supervisor を採用する。

### PID のみで process を識別する

**不採用。**

PID reuse により、無関係な process を操作する可能性がある。

以下を組み合わせて identity を確認する。

- PID
- Start time
- UID

### Codex thread を CWD や時刻だけで推測する

**不採用。**

誤った thread へ以下の capability を与える可能性がある。

- Attach
- Stop

binding を一意に証明できない場合は、unbound のまま扱う。

### 単発の redraw signal を送る

**不採用。**

underlying PTY size に変化がない場合、CLI 側が redraw 不要と判断できる。

実際の PTY size を一度変更して元へ戻す方式を採用する。

### Claude session を native state 上で Rename する

**不採用。**

既存 background session を in-place で改名する supported operation がない。

resume 時に name を指定する方式では別 session が生成されるため、agentsctl-local overlay を採用する。

### Claude transcript を直接編集する

**不採用。**

以下の問題がある。

- provider が所有する内部形式への依存
- native daemon との write conflict
- undocumented format への coupling

agentsctl は Claude transcript を書き換えない。

### Archive と Stop を同じ lifecycle action とする

**不採用。**

両者の意図が異なる。

- Stop: process を終了する
- Archive: Agent View から除外する

active session を整理する場合は、先に明示的に Stop する。

Archive は inactive session の visibility を変更する操作として扱う。

## Open Questions

現時点で未解決の論点はない。
