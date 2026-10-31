# antic のトップレベル仕様、コアの設計と検証

背景調査: `reports/状態遷移仕様 E2Eテスト オラクル 文献調査.md`

## 0. 方針

- 仕様は**望ましい振る舞い**を書く。実装との差分は不具合として扱い、§9 に記録する。
- テストは**実環境だけ**で行う。このホスト、普段のユーザー、実物の systemd --user / KVM / Nix / cloud-hypervisor を使い、モックは使わない。
- 仕様は純粋な Go の状態遷移モデル `World` として書く。オラクルも仕様の一部とする。
  - `Post`：コマンドを実行したときに許容される結果の集合
  - α_obs：実機の観測を `World` に写す抽象化関数
  - プローブ：各状態成分の観測関数

### 対象コマンド（フェーズ 1）

- `guest create / apply / install / uninstall / start / stop / restart / rm`
- `guest set / forward / unforward`
- `guest mount / unmount`
- `guest edit`（ネットワークポリシー `mode` / `default` / `allow` / `deny` の変更に使う。§3.1）
- `run`（プローブにも使う）

### フェーズ 2 以降

- `ps` / `kill`
- flake ゲスト（カーネル変更を伴うもの）
- 並行実行
- 故障注入

### 対象外

`doctor`、`update`、`completion`、`skill`、`cache`、`logs`、`shell`

## 1. 仕様全体を貫く原則

**P1 失敗時の原子性（例外なし）**
各コマンドは、対象要素（ゲストまたは引数）ごとに原子的である。成功すれば `Post` が定める状態になる。失敗すれば exit 125 で、その要素については `World` が変わらない。

- 仕様に例外は設けない。ロールバックにも失敗して旧状態に戻せなかった場合、それは**仕様違反**であり、許容される結果ではない。テストは不合格になる。
- この原則から、実装には **prepare → commit** の 2 相構造が要求される。決定的に失敗しうる検査を、すべて旧状態を壊す前に済ませる。対象は、設定の検証、ビルド、host port の空き、VM 設定の生成など。commit 後に起こりうる失敗は環境要因だけに限られるようにする。
- 実装側で必要になる作業:
  - 設定ファイルの書き戻し
  - システムプロファイルのロールバック
  - 起動していたゲストを旧設定で再起動すること

**P2 複数引数**
要素が互いに独立した複数引数コマンド（`start` / `stop` / `restart` / `rm` の NAME...、`mount` / `unmount` の DIR...）は、要素を先頭から順に処理し、最初の失敗で中断する。それより前の要素は成功したまま残り、それより後の要素は処理しない。

`install` / `uninstall` の PKG... と `forward` / `unforward` の PORT... は、要素ごとには分けない。コマンド全体を 1 つの原子的な要素として扱う（ビルドも再起動も 1 回だけ行う）。

**P3 嘘をつかない**
exit 0 で返すのは、要求どおりの状態になったときだけである。要求と違う状態のまま成功を報告してはならない（D2 がこれに当たる）。

**P4 冪等性**
目標状態にすでにある場合は exit 0 で何もしない。対象は `start`（Running）、`stop`（Off または未作成）、同じ mode での `mount`、`install`（パッケージ導入済み）である。

**P5 フレーム条件**
コマンドは引数で指定したゲスト以外の状態を変えない。ユーザーの既存ゲストも、ここでいう「それ以外のゲスト」に含まれる。

## 2. パッケージ構成

| パッケージ | 純粋性 | 内容 | import 制約 |
|---|---|---|---|
| `internal/spec` | 純粋 | `World`、`Cmd`、`Post`、`Invariant` | `internal/types` と `internal/domain` のみ。`internal/tea/*` と `internal/host/*` は禁止（arch linter に登録し、トートロジーを防ぐ） |
| `internal/spec/refine`（テストのみ） | 純粋 | α_model と、L1a の refinement 検査 | `spec` と `tea/operation`、`tea/egress` |
| `internal/tea/operation`（新設、§11） | 純粋 | コマンド単位の saga コア（実装側の設計） | `spec` は import しない |
| `e2e/`（build tag `e2e`） | I/O | ドライバ、α_obs、プローブ、rapid の状態機械 | `spec` のみ。antic は**ビルド済みバイナリ**として実行する |

## 3. 状態 `World`

```go
type World struct {
    Guests   map[Name]Guest   // キーが存在する ⇔ 設定 TOML が存在する
    External map[Name]Opaque  // 実行開始時に存在した利用者のゲスト。不変（P5）
}

type Guest struct {
    Config Config
    Inst   Option[Inst]       // None ⇔ 未作成（STATE "-"）
}

// 永続化されている「望ましい状態」
type Config struct {
    Packages Set[Pkg]
    Shares   Map[Path, Mode]  // Mode = RO | RW。Path は symlink 解決後の値
    Res      Resources
    Ports    Set[Port]        // Port = {Host, Guest uint16; Proto TCP|UDP|Both}
}
type Resources struct {
    CPUs    int
    Memory  Size
    Net     NetPolicy        // §3.1
}

type Inst struct {
    Installed Sys             // 次回起動で使われるシステム
    Power     Power
}
type Off struct{}
type Running struct {
    Booted Sys                // いま動いているシステム
    Res    Resources          // 起動時の設定値
    Ports  Set[Port]          // 起動時の転送設定
    Mounts Map[Path, Mount]
}
type Mount struct { Mode Mode; Origin Boot | Runtime }

type Sys struct { Packages Set[Pkg] }  // ストアパスの同値類
```

### 3.1 ネットワークポリシー（案 B：直交化した意味論）

現状は `allow` に「例外として穴を開ける」と「許可リスト方式に切り替える」の 2 つの意味が重なっている。案 B ではこれを分け、各設定の役割を次のように 1 つずつに限る。

- `mode`：基本の到達集合を決める
- `default`：既定で許可するか拒否するかを決める
- `allow` / `deny`：例外を足す・引く

```go
type NetPolicy struct {
    Mode    Internet | LAN | None
    Default Open | Closed            // TOML: default = "allow" | "deny"
    Allow   Set[Entry]               // 例外（追加）
    Deny    Set[Entry]               // 例外（除外）
}
type Entry = CIDR | Domain | Wildcard   // "10.0.0.0/8" | "example.com" | "*.example.com"
```

#### 到達可能性 `Reach`

`Reach` は、起動時のホストネットワーク `H`（ホストのアドレスと on-link サブネット）に対する相対的な述語である。

```
Never(d)        = d ∈ neverAllowed ∪ H.Addrs        -- loopback, link-local, multicast, ホスト自身…
Private(d)      = d ∈ 私設レンジ ∪ H.OnLink          -- RFC1918, CGNAT, ULA, NAT64, on-link
Base(m, d)      = m = LAN ∨ (m = Internet ∧ ¬Private(d))
Learned(d)      = d は、Allow のドメインを、TTL 内に DNS フィルタ経由で解決して得た IP

Reach(p, d) = p.Mode ≠ None
            ∧ ¬Never(d)
            ∧ d ∉ p.Deny.CIDRs
            ∧ ( d ∈ p.Allow.CIDRs                                   -- 静的例外は Base より強い
              ∨ ( Base(p.Mode, d)                                    -- DNS で得た IP は Base を越えない（rebinding 対策）
                ∧ (p.Default = Open ∨ Learned(d)) ) )

Resolve(p, n)   = n が p.Deny のドメインに一致する ⇒ 解決を拒否する
                  p.Default = Closed ∧ n が p.Allow のドメインに一致しない ⇒ 解決を拒否する
                  それ以外 ⇒ 上流の DNS に従う
```

#### 設計判断

- **`lan` は糖衣構文として残す（暫定）。** `lan` は `internet` に `allow = [私設レンジ, on-link]` を足したものと等価である。ただし on-link は起動時に動的に決まり、静的な CIDR では書けない。そのため mode として残す方が素直である。
- **`default = "allow"` のときの domain allow は設定エラーにする**（P3）。この組み合わせでは、IP 層での効果がない。public の宛先は最初から届き、private の宛先は rebinding 対策で開かないためである。効果のない設定が成功してしまうと、利用者に誤解を与える。
- **移行規則**：`allow` が空でないのに `default` が省略されている設定は、設定エラーにする。現行の意味（許可リスト方式）と B の意味（Open に例外を足す）が食い違うためである。黙って B として解釈すると、**到達範囲が知らないうちに広がる**。セキュリティ上、それは許されない。エラーメッセージで `default = "deny"` の明記を促す。
- **deny のドメインは DNS 層の性質である。** IP を直接指定した接続までは防げない。この限界は README の Caveats に書かれている CDN 共有 IP の問題と同じ種類であり、仕様にも明記する。

`Invariant` への追加:

7. `Running.Res.Net` は起動時の値を保持し、`Reach` は起動時の `H` を使って評価する。

### 設計判断

- **3 層に分ける。** `Config`（望ましい状態）、`Installed`（次回起動するもの）、`Running`（いま動いているもの）を分けて持つ。こうすると、未 apply、再起動待ち、unmount したが boot 時のマウントがまだ残っている、といった状態を区別できる。
- **`Running.Res` と `Running.Ports` は起動時の値を持つ。** P1 から、Running 中の `set` や `forward` が成功した直後は `Running.Res == Config.Res` が成り立つ。それでも両者を分けて持つのは、プローブ（`nproc` など）の検査対象が `Running` 側だからである。
- **`Switching` は持たない。** `apply` は原子的な遷移として扱う。

### Invariant(World)

1. `Config.Shares` のパス同士は重ならない。`Running.Mounts` のパス同士も重ならない。
2. Runtime マウントは、必ず `Config.Shares` に同じ mode で載っている。
3. 全ゲストの `Config.Ports` の間で、同じ host port と同じ proto の組が重複しない（ゲストをまたいで検査する。D7）。
4. `Running.Ports == Config.Ports` かつ `Running.Res == Config.Res`。これは P1 と、`set` / `forward` が即時に再起動することから導かれる。
5. `External` は実行開始時の値から変わらない。
6. RW でマウントされた共有の中でも、ホスト側に既に存在する保護パス（`.git`、`CLAUDE.md` など。README が列挙しているもの）は、ゲストから書き込めない。

## 4. コマンドと Post

```go
type Outcome struct { World World; Exit ExitClass }  // ExitClass = Ok | Fail | Cmd(n)
func Post(w World, c Cmd) []Outcome
```

P1 により、失敗時の結果は常に「`World` 不変かつ `Fail`」である。そのため `Post` は、ほとんどのコマンドで高々 2 要素の集合になる。

- 事前条件が偽：`{(w, Fail)}`
- 事前条件が真：`{(w', Ok)}`。環境要因の失敗もありうるので、`(w, Fail)` も許容する

ただし、故障を注入していない実行で `(w, Fail)` になった場合は**要調査**として記録する。`Ok` の側を通った割合を網羅の指標として扱う。

表記: `g = w.Guests[n]`、`cls(C) = Sys{C.Packages}`、`boot(C) = Running{cls(C), C.Res, C.Ports, C.Shares をすべて Boot で}`

| コマンド | Reject 条件 | 成功時の World' |
|---|---|---|
| `create n [-p P] [--cpus/--memory/--network/--port]` | 名前が不正 / `g.Inst` が Some / **設定は既にあるのにフラグを指定した**（D3） | 設定がなければ既定値と P で作る（P が空なら `{git}`）。続けて `Inst{cls, boot(Config)}` |
| `apply n` | 未定義 | `Installed := cls(Config)`（未作成なら Off で作成する）。Running のときは `Booted := cls(Config)` |
| `install n P…` / `uninstall n P…` | 未定義 / flake ゲスト / パッケージ名が不正 | `Config.Packages ∪= P`（`uninstall` は `−= P`）。続けて `apply` と同じ遷移 |
| `set n flags` | 未定義 / フラグがない / 値が不正（cpus < 1、memory の書式、未知の network mode） | `Config.Res` を更新する。Running なら `boot(Config)` で再起動する（Runtime マウントは Boot に変わる） |
| `edit n`（`EDITOR` に、決めておいた内容を書き込むスクリプトを渡す） | 未定義 / 編集後の設定が不正（§3.1 の検証を含む） | 設定を新しい内容に置き換える。Running なら、`set` と同じく `boot(Config)` で再起動する。パッケージが変わった場合は `apply` も行う（D9） |
| `forward n PORT… [-p proto]` | 未定義 / 書式が不正 / 同じゲストまたは**他のゲスト**と host port と proto が衝突する | `Config.Ports ∪= {p}`。Running なら `boot(Config)` で再起動する |
| `unforward n PORT…` | 未定義 / 設定されていない | `Config.Ports −= {p}`。Running なら再起動する |
| `start n…` | 未定義 / 未作成 | Off なら `boot(Config)`。Running なら何もしない |
| `stop n…` | 未定義 | Running なら Off。Off や未作成なら何もしない |
| `restart n…` | 未定義 / 未作成 | `boot(Config)` |
| `rm -y n…` | 未定義 / 端末がなく `-y` もない | `delete(Guests, n)` |
| `mount n [--ro] d…` | 未定義 / ポリシー違反（ディレクトリでない、`$HOME` の外、`$HOME` そのもの、deny 対象と重なる）/ 他の共有と重なる / **同じパスに別の mode で設定済み**（D2） | 既に同じ mode なら何もしない。新規なら `Config.Shares[d] = m`。Running なら `Mounts[d] = {m, Runtime}` |
| `unmount n d…` | 未定義 / 設定されていない | `delete(Config.Shares, d)`。Running のとき、Runtime マウントなら `delete(Mounts, d)`。Boot マウントなら Mounts は残す（再起動まで有効） |
| `run n -- cmd` | 未定義 / 未作成 | Off なら先に `boot(Config)`。`Exit = Cmd(cmd の終了コード)` |

### 再起動を伴うコマンドの失敗

対象は `set`、`forward`、`unforward`、Running 中の `apply` である。新しい設定での再起動に失敗した場合は、P1 に従い、旧設定と旧システムで再起動し直して `(w, Fail)` にする。旧状態に戻せなかった場合に許容される結果はなく、不合格になる。

## 5. 観測と α_obs

### 構造の観測（CLI の出力）

| 観測 | 対象 | 写像 |
|---|---|---|
| `antic guest ls --json` | 全ゲスト | `state` の `"-"` / `"stopped"` / `"running"` を、それぞれ未作成 / Off / Running に写す。`"error"` は不合格とする。`External` は、名前と state が変わっていないことだけを見る |
| `antic guest show n --json` | 管理しているゲスト | `packages` / `cpus` / `memory` / `network` / `ports` は `Config` に写す。`system` は、Off なら `Installed`、Running なら `Booted` に写す（対応表で同値類に戻す）。`shares[]` は `Config.Shares ∪ Mounts` に、`mounted` は `Mounts` に写す |
| ホスト側の systemd 状態 | 常時 | `systemctl --user list-units --all --output=json 'antic-*'` で、Off / 未作成 / rm 済みのゲストのユニットが残っていないことを確かめる |

### ゲスト内プローブ（意味の観測）

#### ツール選定の方針

1. **ベースシステムのツールだけを使う。** 実機で確認したところ、標準のゲスト（`builtin [git]`）には NixOS の requiredPackages が入っている。具体的には bash、coreutils、util-linux、iproute2、curl、libressl の `nc`、`getent`、`timeout` である。プローブのためにパッケージを足すと `Packages` のオラクルが汚れるので、追加はしない（`socat` も不要）。
2. **可能な限り、カーネルのインタフェース（`/proc`、`/sys`）を直接読む。** 読んだ内容は、ホスト側の Go コードで解析する。ツールの出力形式やバージョンに依存しないようにするためである。
3. **ホスト側は Go の標準ライブラリだけを使う**（`net.Dial`、`net.Listen`）。外部コマンドは使わない。
4. **前提検査**：系列を始める前に、使うツールがあるかを `command -v` で確かめる。ない場合は不合格にする（暗黙のオラクル）。nixpkgs の更新で requiredPackages が変わったときに、それを検出するためである。

#### プローブ一覧

| 成分 | プローブ | 使うもの | 選んだ理由 |
|---|---|---|---|
| `p ∈ Booted.Packages` | `command -v <bin>` | bash の組み込みコマンド | 追加のツールが要らない。パッケージ名から実行ファイル名への対応表（`jq` → `jq`）を fixture に持つ |
| `Res.CPUs` | `cat /sys/devices/system/cpu/online` を Go で範囲解析する | `/sys` | `nproc` は affinity や cgroup の影響を受け、VM に割り当てた vCPU の数と一致しない場合がある |
| `Res.Memory` | `cat /proc/meminfo` の `MemTotal` | `/proc` | 許容範囲は `[0.85·M, M]`。カーネルの予約分を差し引くため |
| `Mounts`（存在、mode、種別） | `cat /proc/self/mountinfo` を Go で解析する | `/proc` | `findmnt` の表示形式に依存しない。マウント位置、`ro`/`rw`、fstype `virtiofs`、タグを一度に取れる |
| RW であることの確認 | 一意な名前のファイルを `touch` して `rm` する | coreutils | 実際に書き込めるかを見る。RO の共有では `EROFS` で失敗することを確かめる |
| 保護パスが読み取り専用であること | RW 共有の fixture に `.git/` と `CLAUDE.md` を置いておき、`touch .git/x` と `sh -c ': >> CLAUDE.md'` が失敗することを確かめる | coreutils と sh | README が保証している性質。Invariant に追加する |
| NIC の有無 | `ls /sys/class/net` | `/sys` | `None` なら `lo` だけになる |
| egress（TCP） | `nc -z -w 3 <ip> <port>` | libressl の nc | ベースシステムにある。`-z` / `-w` は OpenBSD 系 nc の標準オプションで、接続できたかどうかが終了コードで分かる。curl は HTTP 層まで巻き込むので使わない |
| ホストの loopback に到達できないこと | ホスト側で Go が `127.0.0.1:R` を listen し、ゲストから既定ゲートウェイの `:R` に `nc -z` する。全モードで失敗することを確かめる | nc と Go | passt が loopback をゲートウェイのアドレスに写すかどうかによらず、ホスト側の待ち受けで確実に判定できる |
| port forward（TCP） | ゲストで `timeout 15 nc -l -N <guest port>` に token を流し込んでおき、ホストの Go が `127.0.0.1:H` から token を読む | nc と Go | 双方向の到達性を token で確認できる。listener の準備ができるまで、ホスト側で再試行する |
| port forward（UDP） | ゲストで `timeout 15 nc -u -l <guest port>` を動かし、ホストの Go が token を送る。`run` の stdout に token が出ることを確かめる | nc と Go | UDP は応答を前提にできないので、受信側の出力で判定する |
| forward していないポート | ゲストで listen したまま、ホストから `127.0.0.1:X` に接続して届かないことを確かめる | nc と Go | 反例側の観測 |

### ネットワークのオラクル（`Reach` の同値類ごと）

宛先は、`Reach` の各項が区別する同値類ごとに、代表を 1 つずつ用意する。プローブは「代表 `d` への `nc -z` の成否 == `Reach(Running.Res.Net, d)`」を判定する。

| 同値類 | 代表 | 用意の仕方 |
|---|---|---|
| Never（ホストの loopback） | ゲストの既定ゲートウェイの `:R` | ホストの Go が `127.0.0.1:R` で listen する |
| Never（ホスト自身） | ホストの LAN アドレスの `:R2` | ホストの Go が listen する |
| Private（LAN） | 既定ゲートウェイ（ルーター）の開いているポート | 開始時に、ホストから TCP 53/80/443 を試して自動検出する。見つからなければ「未検査」と報告する |
| public | `1.1.1.1:443` | 固定 |
| Allow の CIDR で例外にしたもの | 上の LAN 代表と public 代表の `/32` | 設定に書く |
| Deny の CIDR で除外したもの | public 代表の `/32` | 設定に書く |
| Allow / Deny のドメイン | `one.one.one.one`（1.1.1.1 に解決される） | `getent hosts` で解決し、その場で `nc -z` を打つ（Learned の窓の中で検査するため） |

生成する設定の組み合わせは `Mode × Default × (Allow ⊆ 代表集合) × (Deny ⊆ 代表集合)` から選ぶ。設定は `guest edit` で入れる。`EDITOR` には、内容を書き込むスクリプトを渡す。これは利用者が実際に使う経路そのものである。

### 部分観測

- α_obs は部分関数とする。
- Running 中の `Installed` は直接観測できない。Off になったときに遅延検査する。
- Off のゲストには `run` プローブを打たない（打つと start の副作用が起きる）。
- プローブが共有に作ったファイルは、その場で消す。消えたことはホスト側から確かめる。

## 6. 1 ステップの検査手順

```
1. outcomes := spec.Post(model, cmd)
2. 実機で cmd を実行し、exit を得る。タイムアウト T を超えたら不合格
3. O0: panic していないこと、JSON がパースできること
4. O1: exit が outcomes のどれかの Exit と一致すること
5. O2: α_obs(ls, show) に一致する o が outcomes の中にあること
6. O3: o.World で変化した成分をプローブで確かめる（遅延検査の分も含む）
7. Invariant(o.World) とホスト側の確認
8. model := o.World
```

## 7. 生成と網羅

- 生成には rapid の状態機械を使う。約 20% は Reject 条件を満たす入力を意図的に生成する。
- 宇宙は小さく固定する。
  - ゲスト名：`e2e-a`、`e2e-b`
  - パッケージ集合：`{git}` と `{git, jq}`
  - ディレクトリ：`$HOME/e2e-work/` の下に、`A`（`.git/` と `CLAUDE.md` を置く）、`B`、`A/sub`、`B` を指す symlink を用意する。deny の対象として `~/.ssh` も使う
  - ポート：ほかと衝突しにくい高位番号を 2 つ
- 網羅の目安として、`(Phase × Cmd)` の遷移ペアを記録する。
- 失敗した系列は rapid で縮小し、txtar（testscript）形式で回帰テストとして固定する。
- **L1**：`spec` の性質テストと、§8 の refinement 検査。KVM なしで動く。
- **L2**：実機で動かす。1 系列の長さは 10〜20 とする。

## 8. 実装モデルとの関係（L1a）

§11 のコマンド単位コア `tea/operation` を作った後は、α_model の定義域は `operation.Model` になる。`operation.Model` は `Config` と lifecycle を内包するので、`World` 全体に写せる。以下は、`lifecycle` 部分の射影である。

α_model は `lifecycle.Model` を `Inst` に射影する。`Installed` は `Installed` に、`Boot` は Running / Off に写す（`Candidate` は無視する）。`Shares[].Mount` は `Mounts` に写す。

この射影のもとで、次を検査する。

`α_model(next) == α_model(prev) ∨ α_model(next) ∈ Post(α_model(prev), cmd)`

## 9. 望ましい仕様と現状の差分（実装側の課題）

| # | 現状 | 望ましい仕様 | 根拠 |
|---|---|---|---|
| D1 | switch に失敗してロールバックした後も、システムプロファイルは新しい構成のまま残る | P1：`Installed` も元に戻す | `ops.go:204`、`nixbuild.go:121` |
| D2 | **（E2E で訂正）** コードを読んだ段階では「何も変わらず exit 0」と予想していた。実機では exit 125 で拒否され、設定も変わらず、観測上は仕様どおりだった。ただし拒否の理由は「同じパスの共有が重なる」で、本来の理由（mode の不一致）とは違う。設定ファイル上の `~/` 表記とフルパスを比べているため、既存の共有を同一パスと判定できていない（D16 を参照） | Reject する（P3）。理由のメッセージも正しくする | `edit.go:118` の `AddShare` |
| D3 | 設定が既にある状態で `create` にフラグを渡すと、フラグを黙って無視する | Reject する（P3） | `ops.go` の `Manager.Create` |
| D4 | lifecycle の Model から、boot 時にマウントした共有が消える | lifecycle 側で `Config` と `Mounts` を分ける | `MsgUnmountShare` |
| D5 | `install`、`set`、`forward` が失敗しても、設定ファイルの変更は残る | P1：設定ファイルを書き戻す。再起動に失敗したら、旧設定で再起動し直す（例外なし）。2 相（prepare → commit）の構造に改める | `pkg.go`、`port.go`、`guestcmd.go` の `set` |
| D6 | 未作成ゲストへの `stop` は成功する | 望ましい仕様と同じ（P4） | — |
| D7 | 他のゲストと host port が衝突しても、`forward` の時点では検出しない | Reject する（Invariant 3） | `edit.go` の `AddPort`。`start` の時点で検出しているかは要確認 |
| D8 | `start` / `stop` / `restart` は全ゲストを処理してからエラーを返し、`mount` / `rm` は最初の失敗で中断する | P2：すべて最初の失敗で中断する | `guestcmd.go` の `eachGuest` |
| D9 | `guest edit` は、Running 中でも再起動しない。新しい設定は次回の start まで反映されず、`Config` と `Running` が食い違ったままになる | `set` と同じく、即時に反映する（Invariant 4 を守る） | `misc.go:19` |
| D10 | `guest edit` で不正な設定を保存すると、ファイルは不正なまま残り、exit 125 になる | P1：元の内容に書き戻す | `misc.go:19` |
| D12 | `EDITOR` に引数付きの値（例: `omarchy-launch-editor --inline`）が入っていると、値全体を 1 つの実行ファイル名として扱い、起動に失敗する | git などと同じ慣習に合わせ、`sh -c '$EDITOR "$@"' sh <path>` の形で起動する（`VISUAL` も考慮する） | `misc.go:38` の `exec.Command(editor, g.Path)` |
| D11 | **（段階 4 で修正済み）** `allow` が 1 件でもあると、許可リスト方式になる（2 つの意味が重なっている） | 案 B：`default` を明示させる。省略して `allow` を書いた設定は、移行エラーにする | `network.go` の `nftRules` |
| D13 | **（修正済み）** `start` / `restart` / `create` が成功を返した直後の数秒間、`antic run` が `vsock port 22: EOF`（exit 125）で失敗する。エージェントの Hello が返す `Ready`（`antic-ready.target`）が、セッションソケットの待ち受け開始より先に成立していた | `start` が成功したら、`run` は直ちに使える | `nix/module.nix` の `antic-ready.target` を `antic-session.socket` の後に並べ、`Wants` にも加えた。E2E の最初の実行で検出 |
| D14 | **（修正済み）** 現在の HEAD では、標準構成のゲストをビルドできない。`4616b9b` でエージェントが `internal/domain`、`internal/tea`、`internal/types` を import するようになったが、`embed.go` の `//go:embed` が更新されていなかった | ビルドできる | `embed.go` に追記した。エージェントが import する全パッケージが埋め込まれているかを検査する `TestGuestFSHasAgentImports` を追加した |
| D15 | **（修正済み）** `/nix` のないホストで rootless Nix を使っていると、ゲストを起動できない。共有ユニットとネットワークユニットが `Failed to set up mount namespacing: /nix: Permission denied` で失敗する（`b2012a0` 以降） | 起動できる | `Manager.confine` は、ホストに `/nix` がある場合にだけ Nix ルートを `/nix` に bind するよう改めた。`/nix` がなく、`/nix/store` にリンクされた libexec ヘルパーを使う構成は、別途の課題として残る |
| D16 | 同じ mode で同じディレクトリを再度 `mount` すると、冪等にならず exit 125 になる。原因は D2 と同じで、パス表記の比較が誤っている | P4：何もせずに exit 0 | `edit.go:118` の `AddShare` |
| D17 | **（修正済み）** 設定ファイルが読み込めないゲストは `guest rm` で削除できない。インスタンスの状態ディレクトリが残り、同じ名前で `create` すると「already exists」になる。さらに、壊れた設定ファイルが 1 つでもあると、`guest ls` が全体として失敗する | モデルの外の状態（手で壊した設定）だが、`rm` と `ls` は壊れた設定にも耐えるべき | `Manager.Get`、`guest ls`。E2E の片付けでは、最小限の有効な設定で上書きしてから `rm` することで回避している |
| D18 | **（修正済み）** PTY なしの `antic run` が、ゲストの起動直後などに exit 0 のまま出力を失うことがある（E2E の障害注入テストで、CPU 数プローブが空文字列を返して検出） | コマンドの出力は欠けない（P3） | ゲスト側の `internal/guest/session` が、出力のパイプを読み切る前に `cmd.Wait` を呼んでいた。`os/exec` の規約では、Wait はパイプを閉じるので未読の出力を失う。PTY なしの場合は、読み切ってから Wait するように改めた |
| D19 | **（修正済み）** `guest set --cpus 0` が成功を返し、ゲストは 2 CPU になる。設定ファイルの `cpus = 0` は「既定値」を意味するため。殻と仕様の差分 PBT で検出した | Reject する（P3） | `Draft.SetResources` は、指定された値を `domain.ParseCPUCount` で検査するようにした |

## 10. 実行基盤

- このホストで、普段のユーザーのまま、実際の antic バイナリを実行する。
- テストで作るゲストは `e2e-` で始まる名前に限る。既存のゲストは `External` として不変であることを検査する。
- テスト用ディレクトリは `$HOME/e2e-work/` に置き、`antic.toml` のないディレクトリから実行する。
- `stop --all` は系列生成から外す。
- テストの開始時と終了時に、`e2e-*` で始まるゲストを掃除する。
- egress のプローブで使う LAN の代表は、既定ゲートウェイを自動で検出して決める（§5）。見つからない場合や、テスト中にホストのネットワークが変わった場合（`H` の変化として検出する）は「未検査」と報告し、合格扱いにはしない。

## 11. 実装側のアーキテクチャ：コマンド単位の Sans-I/O コア

### 11.1 現状と問題

| 部分 | 純粋なコアの使われ方 |
|---|---|
| `tea/control`（ゲストエージェント） | Model を保持し、すべての判断を effect として実行している（十分） |
| `tea/egress`（DNS プロキシ） | 同上（十分） |
| `tea/lifecycle`（ホストでのゲスト操作） | `apply` / `mount` / `unmount` の一部でしか使っていない。`MsgStart` / `MsgStop` は未使用。`create` / `rm` / `set` / `forward` / `edit` と設定の書き込みはモデル化されていない。`Update` が返した Model は捨てられ、毎回ディスクから組み立て直している。重なりの検査やロールバックの判断は殻（シェル）側に残っている |
| `nftRules` | 純粋な関数だが `host/guest` に置かれており、仕様と照合されていない |

D1〜D12 のほとんどは、判断が殻側に漏れていることから生じている。

### 11.2 方針

ホスト側の 1 コマンドを、純粋なコア上の **saga（補償つきトランザクション）** として表す。殻は effect を実行して結果を返すだけの**汎用インタプリタ**にする。

```
internal/tea/operation   （仮称。arch linter には rank 0 の純粋層として登録する）

Model = {
    Snap    : 観測した現状。Config、lifecycle.Model、ホストの網 H、ホストのポート使用状況
    Plan    : 実行中のコマンドの計画
    Stage   : Prepare | Commit | Compensate | Finalize | Done(Ok|Fail)
    Undo    : commit 済みステップの逆操作のスタック
}

Msg    = MsgCommand{Cmd}                   -- CLI から
       | MsgResult{Step, Ok(data) | Err}   -- effect の結果（ビルドしたシステム、割り当てた ID など）

Effect = Prepare 系   : Resolve(path) / Build(cfg) / CheckPorts / RenderVMConfig / RunEditor(tmp) / Validate
       | Commit 系    : WriteConfig / SetProfile / AssignInstance / StopVM / StartVM / Hotplug /
                        AgentSwitch / AgentMount / WriteBoot ...
       | Finalize 系  : GC / DeleteTrash  （取り消せない操作は、ここに限る）
       | Report       : Log / Exit(code)
```

#### 規則

1. **Prepare**：決定的に失敗しうる検査とビルドを、すべてここで済ませる。この段階では、観測できる状態を一切変えない（P1 の prepare → commit）。
2. **Commit**：各ステップは、成功したら逆操作を `Undo` に積む。ステップ k で失敗したら Compensate に移り、`Undo` を逆順に実行する。
3. **Compensate**：補償に失敗した場合の扱いは**定義しない**。`Stage = Broken` は到達不能でなければならない（P1 に例外を設けないため）。補償は冪等で再試行可能にし、Commit の順序は取り消しにくい操作ほど後ろに置く。この性質は L1a で検査する。
4. **Finalize**：取り消せない操作（GC、古い世代の削除、ゴミ箱の削除）は、全ステップの commit が成功した後にだけ行う。ここでの失敗は World に影響しないので、ログを出すだけにする。
5. **P2**：複数引数のコマンドは、要素ごとに独立した saga を順に流し、最初の Fail で止める。
6. **殻の制約**：`host/guest` は effect の型で分岐して実行するだけにし、Model を見て判断してはならない。arch linter では「`host/guest` は `tea/operation` の Model のフィールドを参照しない」という検査を入れる。

### 11.3 コマンドごとの計画（例）

| コマンド | Prepare | Commit（逆操作） | Finalize | 解消する差分 |
|---|---|---|---|---|
| `apply`（Running） | `Build(cfg)` → sys | `SetProfile(sys)`（`SetProfile(old)`）→ `AgentSwitch(sys)`（`AgentSwitch(old)`）→ `WriteBoot(sys)`（`WriteBoot(old)`） | GC | D1 |
| `install` / `uninstall` | 新しい設定を**メモリ上で**作り、`Validate` と `Build` を済ませる | `WriteConfig(new)`（`WriteConfig(old)`）→ 以降は `apply` と同じ | GC | D5 |
| `set` / `forward` / `unforward`（Running） | `Validate`、`CheckPorts`（他ゲストとの衝突も見る）、`RenderVMConfig(new)` | `WriteConfig(new)`（`WriteConfig(old)`）→ `StopVM`（`StartVM(old)` のあと Runtime マウントを `Hotplug` で**付け直す**）→ `StartVM(new)`（`StopVM`） | — | D5、D7 |
| `edit` | `RunEditor(一時コピー)` → `Validate`（§3.1 の移行規則も含む）→ 差分から計画を作る（`set` や `apply` の合成） | 差分に応じて `set` と `apply` の commit 列を並べる | GC | D9、D10、D12（エディタは `sh -c` で起動する） |
| `mount` | `Resolve`、ポリシーと重なりの検査、mode の一致検査 | `WriteConfig`（戻す）→ Running なら `StartVirtiofsd`（停止）→ `AddDevice`（`RemoveDevice`）→ `AgentMount`（`AgentUnmount`）→ `WriteBoot`（戻す） | — | D2 |
| `create` | 名前と設定の検査（既に設定があるのにフラグを渡したら Reject）、`Build` | `WriteConfig`（削除）→ `AssignInstance`（ディレクトリを削除）→ `SetProfile` → `StartVM`（`StopVM`） | — | D3 |
| `rm` | — | `StopVM`（`StartVM`）→ 設定とインスタンスディレクトリを**ゴミ箱へ rename**（rename で戻す） | ゴミ箱を削除 | （P1 を rm にも適用） |

`set` の逆操作を「旧設定での再起動」だけで済ませると、Runtime マウントが Boot マウントに変わってしまう。これは `Mount.Origin` という観測できる違いになるので、補償では付け直しまで行う。こうした点は、コアを書くことで曖昧さなく決まる。

### 11.4 egress のルール

`nftRules` を、`tea/egress` 内の**ルール中間表現**（順序付きの `match → verdict` 列）の生成と、`host` 側の nft テキストへの単純な印字とに分ける。

- 中間表現は純粋に評価できる。そのため L1b で、代表宛先の全同値類について `Eval(IR, d) == spec.Reach(p, d)` を検査できる。
- 印字が正しいかどうかは、L2 の実機プローブで確かめる。

### 11.5 検証の層（改訂）

| 層 | 対象 | 方法 | KVM |
|---|---|---|---|
| L0 | `spec` 単体 | `Invariant` が保存されること、`Post` が空にならないこと | 不要 |
| L1a | `tea/operation` と `spec` | rapid でコマンド列を生成する。各 effect の結果を ok と err で**網羅的に故障注入**し、最終的な α_model(Model) が `Post` に含まれること（P1）と、`Broken` に到達しないことを検査する | 不要 |
| L1b | egress の中間表現と `spec.Reach` | 同値類ごとに評価して照合する | 不要 |
| L1c | `tea/lifecycle` と `tea/control` の既存の性質 | 既存のテスト | 不要 |
| L2 | 実機の E2E | §5〜§7 | 要 |

`spec` は `tea` を import しない。コアと仕様は独立に書き、L1a で突き合わせる（トートロジーを避けるため）。

### 11.6 フェーズ 2 で扱うこと：クラッシュ時の一貫性

antic が commit の途中で kill された場合に備え、コアは write-ahead のジャーナルを effect として出す（段階 5 の準備で実装済み。§13）。

- **開く**：最初の commit ステップの前に `EffectJournalBegin` を出す。シェルはコマンド（`Op`）と旧・新の実体を永続化する。
- **意図の記録**：各 commit ステップの直前に `EffectJournalIntent{Pos}` を出す。`StepValid` は、意図を記録した直後にしか commit ステップが走らないことを検査する。
- **閉じる**：全 commit が成功したら `EffectJournalClear` を出し、成功するのはその後である。消せなかった場合は、残ったジャーナルが後で成功を取り消してしまう（P3 に反する）。そのため、補償に入って失敗として報告する。補償を終えたときも `EffectJournalClear` を出す。消せなかった場合も失敗と報告し、ジャーナルは次のコマンドが冪等にもう一度戻す。
- **回復**：シェルはロックを取ったあとジャーナルが残っていれば、`MsgCommand` の代わりに `MsgRecover{Op, Last}` を送る。コアは記録された意図を逆順に戻し、ジャーナルを消して `Recovered` で終わる。シェルはその後で状態を読み直し、自分のコマンドを計画する（PlanUnderLock）。
- **冪等性の要求**：逆操作は、元のステップが実行されたかどうかに関わらず資源を Old にしなければならない。記録された意図は、実行されなかったかもしれないからである。
- **Broken**：補償が失敗したときはジャーナルを消さない。次のコマンドの回復が、もう一度戻すことを試みる。

## 12. 段階計画（改訂）

1. **仕様**：`internal/spec`（`World`、`Post`、`Reach`、`Invariant`）と L0。arch linter に登録し、`tea` と `host` の import を禁止する。
2. **E2E の最小版**：`e2e/` のドライバ、α_obs、O0〜O2 を作り、**現状の実装に対して流す**。D1〜D12 を既知の失敗として記録し、書き換え前の安全網と、後退の検出に使う。
3. **コマンド単位のコア**：`internal/tea/operation` の Model、Msg、Effect と saga を作り、§11.3 の計画を実装する。L1a で故障注入つきの検査をする（殻にはまだ手を入れない）。
4. **egress**：ルール中間表現への分離と L1b。
5. **殻の書き換え**：`host/guest` を effect インタプリタにし、CLI の全コマンドをコア経由にする。lifecycle は D4 を直したうえでコアに組み込む。手順 2 の既知の失敗が、すべて通るようになることを確かめる。
6. **E2E の完成**：O3 のプローブ（ネットワークを含む）、rapid による系列生成と縮小、txtar での回帰固定。
7. **フェーズ 2**：クラッシュ時の一貫性（§11.6）、`ps` / `kill`、flake とカーネル変更、並行実行。

## 13. 実装状況

### 段階 1：`internal/spec`（完了）

| ファイル | 内容 |
|---|---|
| `world.go` | `World`、`Guest`、`Config`、`NetPolicy` とそれぞれの正準集合（`SortedSet`） |
| `config.go` | `Env`（ホストの事実）と `RawConfig`、設定の検証（案 B の移行規則を含む） |
| `reach.go` | `Reach`、`Resolvable`（§3.1） |
| `cmd.go` | フェーズ 1 の 15 コマンド。引数は CLI に渡す生の値のまま持つ |
| `post.go` | `Post(World, Env, Cmd) []Outcome`。P2 の畳み込みは `fold` で書いた |
| `invariant.go` | `Invariant(World)` |
| `spec_test.go` | L0 の検査（下記） |

#### L0 の検査

- 幅優先探索を深さ 4 まで行い、1,875 状態・20,033 遷移について次を確かめる。
  - `Post` が空でない
  - 遷移後の World が `Invariant` を満たす
  - P1（単一要素のコマンドが失敗したら World は不変）
  - P5（外部ゲストと、名指ししていないゲストは変わらない）
  - Reject が最終の結果であること、失敗時の exit が 125 であること
- D2、D3、D6、D7、D9、D10、D11、P2、`run` の exit 伝播について、シナリオテストを置いた。
- `Reach` と `Resolvable` を真理値表で確かめる。
- ミュータント 3 種（停止時にマウントを残す、rm で外部ゲストを消す、Reject で状態を進める）が、いずれも検出されることを確認した。

#### arch linter

- `internal/spec` を純粋層（rank 0）として登録した。`checkPureFiles` の対象にも加えた。
- `spec` から `tea` への import を禁止した。
- テスト以外のパッケージが `spec` を import することも禁止した。

#### domain

- `PackageName`、`PortNumber`、`CPUCount`、`MemorySize` を追加した。
- `config.ValidPackage` は `domain.ParsePackageName` に委譲するよう改めた（SSOT）。

#### 設計からの変更点

- **`Installed`、`Booted`、`Running.Res`、`Running.Ports` を `World` から外した。** 望ましい意味論（P1 と即時反映）のもとでは、作成済みのゲストのこれらの値は常に `Config` と一致するためである。`Config` と食い違いうる実行時の状態は、`Mounts`（boot 時のマウントが残留するもの）だけである。カーネルを変える apply で再起動待ちの状態が生じるフェーズ 2 で、改めて分離する。
- **`PhaseUncreated` には、コマンドだけでは到達できない。** 設定ファイルが手書きで置かれた、という初期状態からしか生じないためである。E2E では、事前準備（フィクスチャ）として TOML を直接置くことで、この状態を作る。
- **`edit` の扱い**：実行中のゲストでマウント中の共有の mode を変える場合と、残留しているマウントと重なる場合は、再起動の経路に回す。CLI の `mount` で mode を変えようとした場合は、Reject のままにする（D2）。
- **外部ゲストを名指しするコマンドは、モデルの外とする。** 名前を省略した結果、外部ゲストが選ばれる場合も含む。`Post` は `nil` を返し、生成器はこうしたコマンドを出さない。

### 段階 2：E2E の最小版（完了）

`e2e/`（build tag `e2e`、ファイルはすべて `_test.go`）の構成:

| ファイル | 内容 |
|---|---|
| `harness_test.go` | フィクスチャ、`spec.Cmd` から argv と環境変数への変換、TOML の書き出し、片付け |
| `observe_test.go` | α_obs と view の比較。O3 は vCPU 数のプローブだけを実装した |
| `scenarios_test.go` | 1 ステップの検査（O0〜O3）とシナリオ |

#### 実行方法

```sh
nix develop -c sh -c 'export CGO_ENABLED=0 && go build -o ~/.cache/antic-e2e/bin/antic ./cmd/antic &&
  go test -c -tags e2e -o ~/.cache/antic-e2e/e2e.test ./e2e'
cd e2e && ANTIC_E2E_BIN=~/.cache/antic-e2e/bin/antic ~/.cache/antic-e2e/e2e.test -test.v -test.timeout 3h
```

- バイナリは `nix develop` の中で静的にビルドする。テスト本体は、普段のシェル環境から実行する。
  - このホストの rootless Nix では、`nix develop` は内部で別ストアを `/nix` に見せる名前空間の中で動く。その中からは Nix がゲストをビルドするためのサンドボックスを作れない。
  - 動的リンクしたバイナリは、その名前空間の `/nix` を前提にするので、外では起動できない。
- テスト対象の `antic` は `$HOME` 配下に置く。antic は自身のバイナリを、`PrivateTmp` を有効にした systemd ユニットに bind するためである。
- 既知の差分シナリオは、最後のコマンドで差分を再現した場合にだけ skip する。それより前のステップで失敗した場合は、本物の失敗として扱う。差分が解消されたら fail させ、既知マーカーの削除を促す。

#### 実行結果（2 回目、約 5.5 分）

| 結果 | シナリオ |
|---|---|
| 仕様どおり（PASS） | lifecycle、shares、D2、D6 |
| 既知の差分を再現（SKIP） | D3、D5、D7、D8、D9、D10、D11、D12、D16 |

- 外部ゲスト（`dev`、`formal`）は、全ステップで不変だった。
- E2E を動かす過程で、antic 本体に阻害要因が 3 件見つかった（D13、D14、D15）。いずれも修正済みである。
- 実機の観測によって、コードを読んだ段階での予想（D2）を訂正した。新たな差分として D16 と D17 を記録した。

#### 未実装（段階 6 で扱う）

- O3 のプローブのうち、vCPU 以外のもの（パッケージ、マウントと mode、保護パス、ネットワーク、ポート）
- rapid による系列生成と縮小、txtar での回帰固定
- D1（switch 失敗の再現に故障注入が要る）
- D4（lifecycle モデル内部の問題で、E2E では観測できない）

### 段階 3：コマンド単位の Sans-I/O コア（完了）

`internal/tea/operation` を新設した（§11 の saga コア）。TEA の規約（Model / Msg / Effect、`Invariant`、`FrameValid`、`StepValid`）に従い、arch linter を通っている。

#### 設計

- コアは設定の中身を扱わない。扱うのは、コマンドが触る**資源**とその**版**だけである。
  - 資源：設定ファイル、インスタンス、システムプロファイル、実行中システム、電源、ホットプラグした個々のマウント
  - 版：Old、Mid（再起動の途中で停止した状態）、New
- 資源の版は `Plan` と `Pos` から導出し、Model には持たない（SSOT）。
- 入力 `Op` は次の 4 つからなる。
  - コマンドの形：`OpChangeConfig` / `OpCreate` / `OpStart` / `OpStop` / `OpRestart` / `OpRemove`
  - 開始時のフェーズ
  - 変更内容 `Change`：設定、システム、再起動の要否、追加・取り外しする共有
  - ホットプラグ済みの共有
- apply / install / uninstall / set / forward / unforward / mount / unmount / edit は、すべて `OpChangeConfig` として扱う。`run` は `OpStart` の後に実行する。
- フェーズだけで決まる拒否は、コアが Prepare より前に行う。未作成のゲストへの start / restart と、作成済みのゲストへの create がこれに当たる。

#### シェルへの要求（段階 5 で実装する）

1. **複合 effect の原子性**：`StartVM`、`StopVM`、`Hotplug`、`Hotunplug`、`Switch` は、失敗した場合に何も変えてはならない。
2. **`EffectStartVM{Config: v}` は版 v の設定とシステムで起動する**：プロファイルからは起動しない。再起動を補償するときは、プロファイルを戻すより先に旧システムを起動する。そのため、シェルは旧システムのパスを保持しておく必要がある。この点は、L1a のテストで影の状態を設計する過程で判明した。
3. **`Change` は、シェルが旧設定と新設定の差分から求める**：この差分計算は純粋な処理なので、コアか spec の隣に純粋関数として置く案も検討する。

#### 検証

| 検査 | 規模 | 内容 |
|---|---|---|
| L1a `TestSagaAtomicityUnderFaultInjection` | 576 コマンド、3,028 実行 | 全フェーズ × 全変更の組み合わせについて、forward の各 effect と補償の各 effect に失敗を 1 回ずつ注入する。シャドウ状態（テストが独立に持つ effect の意味論）で次を確かめる。成功なら期待した新状態になる。失敗なら初期状態に戻る（P1）。Broken になるのは補償が失敗した場合だけである。Finalize は全 commit の後にしか走らない |
| L1a 精緻化 `internal/spec/refine` | 89 状態、563 遷移 | 仕様の遷移ごとに、コアの計画が触る資源（足跡）と、仕様が変化させる状態成分が一致することを確かめる。フェーズで決まる拒否の扱いも一致することを確かめる |
| ミュータント | 6 種 | 補償時の再ホットプラグ漏れ、補償での New 起動、Hotunplug 漏れ、Switch 漏れ、停止中のゲストへの stop で作業すること。いずれも検出した |

#### 付随する変更

- domain に `StepIndex`、`StepIndexOrder`、`AbsPathOrder` を追加した。
- arch linter に `internal/spec/refine`（テストファイルだけのパッケージ）を登録した。
- `.golangci.yml` の depguard（`tea-pure`）の許可リストに `internal/domain` と `internal/types` を追加した。
  - `4616b9b` 以降、arch linter とは食い違っており、既存の `tea` パッケージも depguard の違反になっていた。
  - この修正で、既存の `tea` パッケージに対する他の指摘（exhaustive と staticcheck、計 8 件）が表に出るようになった。新しく追加したコードへの指摘は 0 件である。

#### 残課題（Broken）

補償が恒久的に失敗した場合（Broken）は、仕様違反として報告するだけである。到達しないこと自体は保証できない。これを減らす手段は 2 つある。補償 effect を冪等にしてシェル側で再試行すること、そしてジャーナルによる再開である。後者は段階 5 の準備で入れた。Broken のときはジャーナルが残り、次のコマンドが回復を試みる。

### 段階 4：egress のルール中間表現（完了）

#### 実装

- **ルールの中間表現を `tea/egress` に置いた**：`internal/tea/egress/rules.go` の `Rules(Policy, HostNet) []Rule` と `Eval(rules, d, learned)` は純粋関数である。順序付きの `match → verdict` 列で、最初に一致したルールが判定を決める。`NeverPrefixes()` と `PrivatePrefixes()` もここに移した。
- **ホスト側は書き出しだけにした**：`internal/host/guest/network.go` の `nftRules` が担うのは、配管（転送ポート、DNS の経路）の出力と、中間表現を順に nft テキストへ書き出すことだけである。prefix は常に `netip` の表記で出力する（例: `::ffff:0.0.0.0/96`、`192.168.11.12/32`）。
- **案 B を実装した（D11）**：
  - 設定に `network.default`（`"allow"` / `"deny"`）を追加した。
  - 学習済み IP の集合（dyn set）と最後の reject は、`default = "deny"` のときだけ出力する。DNS フィルタも、`default = "deny"` のときか、ドメインの entry があるときだけ起動する。
  - `tea/egress` の Model に `Default` を追加した。`DomainAllowed` は「deny が優先、`default = "deny"` なら allow に一致したものだけ」を返す。
  - netns ヘルパーに `--default-deny` を追加した。
- **移行規則**：`allow` を書いて `default` を省略した設定はエラーにする。`default = "allow"` のまま `allow` にドメインを書いた設定もエラーにする。手元の設定（`dev`、`formal`、`antic.toml`）には該当するものがないことを確認した。
- **文書**：README、`examples/dev.toml`、SKILL.md（2 か所）に `default` の説明を加えた。

#### 検証

| 検査 | 規模 | 内容 |
|---|---|---|
| L1b `TestEgressRulesRefineReach`（`internal/spec/refine`） | 208 ポリシー、8,736 判定 | mode × default × allow の部分集合 × deny の部分集合の全組み合わせについて、`Eval(Rules(...))` が `spec.Reach` と一致することを確かめる。宛先は各同値類の代表 21 個で、on-link、ホスト自身、public、private、never、IPv4-mapped を含む。learned（学習済みかどうか）の両方の値を試す。DNS フィルタの判定も `spec.Resolvable` と照合する |
| ミュータント | 4 種 | deny と allow の順序を入れ替える、on-link を遮断対象から外す、学習済み IP の許可を LAN 遮断より前に置く（rebinding）、IPv4-mapped を unmap しない。いずれも検出した |
| 単体テスト | — | `network_test` を案 B に合わせて更新した。`default = "allow"` で CIDR を例外にする場合のテストと、`network.default` の移行規則のテストを追加した |
| E2E（3 回目、約 6.5 分） | 15 シナリオ | 新たに `default = "deny"` と、`lan` + 例外の 2 シナリオを加えた。いずれも生成したルールを実機の namespace に読み込み、ゲストが起動することを確かめた。D11 は既知の差分から外れ、PASS になった。残る既知の差分は D3、D5、D7、D8、D9、D10、D12、D16 |

到達性を実機で観測するプローブ（O3 のネットワーク）は、段階 6 で扱う。

### 段階 5 の準備：ジャーナルと形式モデルとの対応（完了）

文献調査（`reports/形式モデルと実装の対応 文献調査.md`）の推奨 1・2 を実施した。結論は「継ぎ目ごとに閉じる」である。

| 継ぎ目 | 方法 | 状態 |
|---|---|---|
| `spec` ↔ `tea/operation` | 足跡の精緻化（`TestCoreRefinesSpec`） | 既存 |
| egress データ ↔ `spec.Reach` | Z3（`egress_smt_test.go`） | 既存 |
| `Saga.tla` ↔ `tea/operation` | TLC 状態グラフの再生（`TestGoCoreRefinesSagaModel`） | **今回** |
| `Saga.tla` ↔ CHC | 手で書き写したもの（規則は action ごとに対応させた） | 検査は推奨 3 で行う予定 |
| コア ↔ 殻 | NDJSON トレースと TLC による照合 | 段階 5 で行う |

#### コアのジャーナル（§11.6）

- Stage を追加した：`Begin`、`Intent`、`Close`、`Abort`、`Recover`、`RecoverClose`、`Recovered`。
- Effect を追加した：`JournalBegin`、`JournalIntent{Pos}`、`JournalClear`。Msg として `MsgRecover{Op, Last Option[StepIndex]}` を追加した。
- `Invariant` と `StepValid` は新しい Stage を扱うようにした。`StepValid` には write-ahead の検査を加えた（commit ステップは、同じ位置の Intent の直後にしか走らない）。
- 位置の計算は `after` / `before` / `undo` にまとめ、添字の算術が散らばらないようにした。

#### Saga.tla の改訂

- Go コアの 1 遷移（effect 1 つとその結果）が TLA+ の 1 action になる粒度にそろえた。
  - 追加した action：`PlanCmd`、`Release`、`IntentFail`、`FinishFail`、`CompensateDoneFail`、`RecoverFail`。
  - 終了は `done` フェーズを経由させ、ロックの解放を `Release` に分けた。
  - 回復は durable なジャーナルを書き換えず、ローカルのスタックで戻すようにした（Go の実装と同じ）。
- 資源を `NRes` と `Res == 1..NRes`、`Plan == [i ∈ Res ↦ i]` にした。CHOOSE による不定の順序をなくし、Go の計画の位置と対応させるためである。
- SagaJournal の結果：3,949 状態、Consistent と Termination が成り立つ。SagaNoJournal と SagaStaleRead では反例が出る。

#### 検証

| 検査 | 規模 | 内容 |
|---|---|---|
| `TestGoCoreRefinesSagaModel` | TLC 3,949 状態・6,660 辺、Go 4,511 状態・7,880 遷移 | TLC が出力した状態グラフ（`-dump dot,actionlabels`）を読む。同じ上限（2 プロセス、3 コマンド、2 クラッシュ）で `operation.Update` を動かす。抽象化関数 `alpha` と対応表 `action`（Go の Stage と結果 → TLA+ の action）を通して、Go の全遷移が stutter か、同じ action の TLC の辺であることを確かめる。逆に、TLC の全状態と全辺に Go 側から到達することも確かめる。つまり、この有限の構成で stutter を除いて等価である。上限が cfg と一致することも検査する |
| ミュータント | 6 種 | ジャーナルを開かない、意図を記録しない、逆操作で New に戻す、回復で最後の意図を飛ばす、ロック前に読んだ状態で計画する、ジャーナルを消す前に成功扱いにする。いずれも検出した。さらに、コアに「回復で最後の意図を飛ばす」バグを直接入れると、`Invariant` と `StepValid` は通る一方で、この検査は検出した |
| `TestSagaRecoversFromCrashes` | 57,598 実行 | 全コマンド × 故障 1 回 × 全クラッシュ点について、ジャーナルが残れば回復で初期状態に戻ることを確かめる。回復自体を任意の点でクラッシュさせ、もう一度回復させた場合も確かめる。ジャーナルが残っていなければ、旧状態か新状態のどちらかになっている |
| L1a（改訂） | 6,720 実行 | ジャーナルの effect も故障注入の対象にした。成功時にジャーナルが残らないこと、Broken のときはジャーナルが残っていて回復できることも確かめる |
| Spacer（改訂） | 約 3 秒（以前は約 18 秒） | Saga.tla の改訂に合わせて CHC を書き直した。不変条件の述語をフェーズごとに分けたことで、改訂後 216 秒かかっていたものが約 3 秒になった。ジャーナルなしの場合は unsat になる |

前提：TLA+ モデルは、逆操作（補償・回復）の失敗と、effect の途中でのクラッシュを扱わない。再生側もこの 2 つを生成しない。前者は L1a の故障注入（Broken）で、後者は殻の原子性の要求（§13 段階 3）で別に扱う。

### 段階 5：殻をコアに移す（mutating コマンドは完了）

各コマンドは次の順で動く。

1. ゲストのロックを取る。前のプロセスのジャーナルが残っていれば、それを回復する（`lockAndRecover`）。
2. ロックの下で設定ファイルを読み、変更案を作る（`config.Draft`）。
3. 計画を立てる（`planChange`）。旧・新の実体を集めて `txn` にまとめ、`operation.Op` を作る。
4. コアの effect を 1 つずつ実行する（`runSaga` / `execute`）。

設定ファイルは、検証済みの変更案として保持し、saga の `WriteConfig` ステップでだけ書く。そのため、失敗すれば元の内容に戻る（D5、D10）。

| 手順 | コマンド | 直した差分 | 主な変更 |
|---|---|---|---|
| A | mount / unmount | D16、D2 の理由、P2 | `Draft.AddShare` / `RemoveShare`。比較は解決済みの実パスで行う。Running なら Hotplug / Hotunplug を冪等な ensure 形にした |
| B | install / uninstall / apply | D1（プロファイルの戻し） | `nixbuild` の `Install` を `SetProfile` / `RemoveProfile` / `GCGuest` に分けた。switch は boot 記録の更新まで含めて 1 effect にした |
| C | set / forward / unforward | D5、D7 | `Draft.SetResources` / `AddPorts`（他のゲストとの衝突を拒否） / `RemovePorts`。再起動が要るかは `config.NeedsRestart` で判定する。`CheckPorts` は、自分が既に転送しているポートを除いて調べる |
| D | start / stop / restart | D8（P2） | `power` が `OpStart` / `OpStop` / `OpRestart` をコアに渡す。`eachGuest` は最初の失敗で中断する |
| E | edit | D9、D10、D12 | 一時ファイルの写しを `sh -c '$EDITOR "$@"'` で編集させる（`VISUAL` を優先する）。終了後、ロックの下で元の内容と変わっていないことを確かめてから置き換える。Running なら `liveShares` でホットプラグできるかを判定し、できなければ再起動する |
| F | create / rm | D3、ゴミ箱 | `NewGuestDraft`（`NewFile`：旧版は「ファイルなし」）。rm は Trash（設定とインスタンス）→ EmptyTrash（finalize）。設定がゴミ箱に入ったまま中断したゲストは、`RecoverOrphan` で戻す |

E2E には故障注入のフックを置いた。`ANTIC_SAGA_CRASH_AFTER=k` を付けると、k 番目の effect を実行した直後に exit 137 で終わる。`TestE2ECrashRecovery` は、実機上でクラッシュ → 回復 → 観測が元の状態と一致することを確かめる。

#### 検証

| 検査 | 結果 |
|---|---|
| E2E（手順 A の後、約 7 分） | 16 シナリオ。D16 と、複数ディレクトリの mount が P2 で止まるシナリオを含めて PASS |
| E2E（手順 B〜E の後、約 8 分） | 18 シナリオ。D5、D7、D8、D9、D10、D12 が既知の差分から外れて PASS。残る既知の差分は D3 だけ |
| E2E（手順 F の後、約 8 分） | 18 シナリオがすべて PASS（既知の差分 0 件）。`TestE2ECrashRecovery` も PASS：mount のクラッシュ点 3 つと、再起動を伴う set のクラッシュ点 3 つ（設定を書いた後、VM を止めた後、新しい設定で起動した後）。いずれも次のコマンドで回復し、観測がクラッシュ前の状態と一致した |
| 単体テスト | `TestDraftShares`、`TestDraftPorts`（D2、D7、D16）、`TestJournalRoundTrip`、`TestLiveShares`、`TestTrashRestore` |

注記：B〜E は同じファイル群にまたがって変更したため、E2E は手順ごとではなく、B〜E をまとめて 1 回実行した。

#### 段階 5 の残りの実施（完了）

| 項目 | 内容 | 結果 |
|---|---|---|
| 故障注入（D1） | `ANTIC_SAGA_FAIL=NAME[:n]` で、n 番目の `EffectNAME` を実行せずに失敗させる。`TestE2EFaultInjection` は Running のゲストで 4 つの場合を試す：install の Switch（D1）、set の再起動での StartVM、mount の Hotplug、forward の JournalClear。いずれも exit 125 で、観測はコマンド前と一致した。実行中のシステムも、停止後にプロファイルが指すシステムも、元のままだった | PASS |
| 形式モデルの一般化 | `Saga.tla` で、コマンドごとに計画の長さ（commit ステップ 0〜NRes 個）を選ぶようにした。`base` と `lastOk` は資源ごとにした。再生では長さごとに実際のコマンドの形を使う（start、stop、mount、設定とパッケージの変更）。CHC も同じ形に直した | TLC 19,727 状態・36,810 辺と完全一致。ミュータント 6 種をすべて検出。Spacer は約 4 秒 |
| 殻のトレース照合（推奨 4） | `ANTIC_SAGA_TRACE` に NDJSON を書き出す（ロックの取得と解放、計画の長さ、各 effect の Stage と結果）。`TestShellTracesRefineSagaModel` は、再生と同じ対応表で TLA+ の action に写し、`SagaTrace.cfg`（計画は最大 5 ステップ）の状態グラフ上で非決定性オートマトンとして辿る。解放せずに終わったプロセスは Crash とみなす。静止点でモデルを初期状態に戻すので、長いトレースも検査できる | E2E の全 24 トレース、1,308 イベントが適合。誤ったトレース 4 種は拒否した |
| D4 / lifecycle | `Shares` と `Inspect` を、設定と boot 記録から直接求めるようにした。`host` は `tea/lifecycle` に依存しなくなった | — |
| D17 | `Manager.All` は読み込めない設定を飛ばす（`ls` はそれらを error として表示する）。`rm` は読み込めない設定のゲストも削除する（設定の中身ではなくバイト列だけを使う）。`TestE2EBrokenConfig` で、実行中のゲストの設定を壊し、`ls`、`rm`、同名での再作成ができることを確かめた | PASS |
| D18（新規・修正済み） | トレース付き E2E の最中に、`antic run` が exit 0 のまま空の出力を返したことで見つけた。原因はゲスト側のセッションで、`cmd.Wait` を出力パイプを読み切る前に呼んでいたこと | 修正後の E2E で確認 |

`tea/lifecycle` パッケージは、host からもゲストのエージェントからも使われなくなったので削除した（D4 はこれで対象がなくなった）。

### 推奨 3・5：手で書いた形式記述の検査（完了）

| 継ぎ目 | 検査 | 結果 |
|---|---|---|
| `Saga.tla` ↔ CHC（推奨 3） | `TestCHCCoversSagaModel`。CHC の規則をデータとして持ち（`chcModel`）、Horn 節と遷移式の両方をそこから出力する。TLC の全辺をロック保持者の視点に抽象化し、同じ action の規則の具体例になっていることを Z3 で確かめる。全規則が少なくとも 1 回使われることも確かめる | 19,727 状態から得た 2,079 本の抽象辺がすべて 25 規則で被覆された。ミュータント 3 種（規則の欠落、回復で New に戻す、Apply のガードを強める）はすべて検出した。限界：検査は 1 ステップ単位なので、その相で陳腐化した変数を誤って読む規則は検出できない（「回復で現コマンドの base を使う」ミュータントは通ってしまった） |
| egress の SMT 生成器 ↔ Go（推奨 5） | `TestSMTGeneratorAgreesWithGo`。生成した式を具体値で評価し、Go と比べる。`smtIn` は `netip.Prefix.Contains` と、全 prefix の境界の前後のアドレスで比べる。`rulesFormula` は `egress.Eval` と、`reachFormula` は `spec.Reach` と比べる | 1,232 件が一致した。`smtPrefix` のビット範囲を 1 つずらすバグを入れると、この検査は検出した。一方、Z3 の等価性証明は両辺が同じ生成器を使うため、このバグを検出できない |

これで、継ぎ目の表（§13「段階 5 の準備」）にあった「手で書き写したもの」「未実装」はなくなった。各継ぎ目は、機械的な検査（証明、モデル検査の再生、トレース照合、差分検査のいずれか）で結ばれている。推奨 6（Gobra による Go コードの直接検証の概念実証）は見送った。Gobra は nixpkgs になく、導入するには GitHub のリリース jar を Nix の derivation に固定し、JRE で動かす必要がある。一方で、コアと TLA+ は状態グラフの完全一致で、殻と TLA+ はトレース照合で、すでに結ばれている。概念実証で得られるものは、この費用に見合わない。純粋関数の帰納的な性質を証明したくなったときに、推奨 7（Dafny を証明専用に使う）と合わせて再検討する。

### 段階 6：E2E の完成（進行中）

#### O3 のネットワーク（`e2e/network_test.go`）

ゲストの中から、ベースシステムの道具だけで到達性を調べる。TCP 接続は bash の `/dev/tcp` で、名前解決は `getent ahosts` で試す。期待値は書き下さず、仕様から計算する。モデルにあるゲストのポリシーと、ハーネスが `net.Interfaces` から独立に求めたホストの網（`spec.HostNet`）を、`spec.Reach` と `spec.Resolvable` に渡す。

| ポリシー | 調べたこと（結果は観測 = 仕様） |
|---|---|
| internet | 1.1.1.1:443 に届く。example.org を解決できる |
| internet、deny `1.1.1.0/24` | 1.1.1.1 には届かない。1.0.0.1 には届く |
| default deny、allow `one.one.one.one` | 1.1.1.1 には届かない。example.org は解決できない。`one.one.one.one` を解決すると 1.1.1.1 が学習され、届くようになる |
| none | 名前解決ができない |
| すべて | ホスト自身の LAN アドレスで待ち受けるリスナーには届かない（Never） |

ホスト自身が 1.1.1.1:443 に届かない環境では、この検査を skip する。

#### 系列の生成と回帰の固定（`e2e/random_test.go`）

- 安定した名前の付いたコマンドのアルファベット（25 種）の上で、種を固定してランダムウォークする（`ANTIC_E2E_RANDOM=N`、`ANTIC_E2E_SEED`、`ANTIC_E2E_LENGTH`）。各ステップは、シナリオと同じく `Post` と観測で検査する。
- 失敗した系列は、失敗が保たれる範囲でコマンドを 1 つずつ貪欲に取り除いて縮める。予算は実行回数で決める。縮めた系列は `e2e/testdata/regress/seed-N.txtar` に固定する。ファイルの中身は、由来と失敗理由のコメントと、`-- commands --` 以下のコマンド名である。
- `TestE2ERegressions` は、固定した系列をすべて再生する。
- rapid は採らなかった。実機では 1 回の実行に数十秒かかるので、再実行を繰り返して縮める rapid の方式は合わない。依存を足すと Nix の `vendorHash` の更新も要る。
- 仕組みの確認：環境全体に `ANTIC_SAGA_FAIL=Hotplug` を入れて流した。6 コマンドの系列が `create mount-A` の 2 コマンドまで縮み、txtar に固定された。故障注入なしで再生すると通った。この確認用に作った固定ファイルは削除した。

#### 形式手法と実コードの溝を PBT で埋める（`pgregory.net/rapid`）

形式モデルの検査は、有限の構成と抽象化の上で行っている。その外側を、純粋なコードに対する性質ベースのテストで覆う。

| 対象（溝） | テスト | 性質 | 確認 |
|---|---|---|---|
| egress：代表点 21 個と自由論理変数による抽象化の外側 | `TestPBTEgressRulesRefineReach`（`internal/spec/refine`） | 差分：ランダムなホスト網、ランダムなポリシー（設定が受け付ける CIDR の任意のリスト）、ランダムな宛先（各 prefix の境界と IPv4-mapped 形式に寄せる）について、`egress.Eval(Rules(...))` = `spec.Reach` | `Eval` の Unmap を外すミュータントは、`::ffff:1.0.0.0` まで縮めて検出した。最初は IPv4-mapped 形式を生成できず見逃したので、生成器を直した |
| 同上（仕様と実装が同じ誤りを共有する場合） | `TestPBTEgressMetamorphic` | メタモルフィック関係（実装側で検査）：deny を足しても到達性は増えない。allow を足しても減らない。default deny ⊆ default allow。learned ⊇ 未学習。エントリの順序に依存しない | — |
| saga コア：TLC の有限構成（2 プロセス、3 コマンド、4 つの形）の外側 | `TestPBTSagaConsistentUnderCrashes`（`internal/tea/operation`） | 状態付き PBT：2〜4 プロセス、コーパスのすべてのコマンド、任意の effect の失敗（TLA+ モデルが扱わない逆操作の失敗も含む）、クラッシュを、50〜400 ステップにわたってランダムに織り交ぜる。各ステップの後に `Consistent` が成り立つ | 「回復で最後の意図を飛ばす」コアのバグを検出した。最初は rapid の `Repeat`（30 ステップ、行動を一様に選ぶ）では到達できなかったので、ロック保持者が取れる行動だけを選ぶ形に改めた |
| 殻の設定編集（`config.Draft`、実ファイルの TOML とポリシー）：形式モデルの外側 | `TestPBTDraftEditsRefineSpec`（`internal/host/guest`） | 差分：停止中のゲストに mount / unmount / forward / unforward / set のランダムな系列を適用する。殻は、仕様が受け付けるものだけを受け付け、仕様の `Config` を意味するファイルを残す | **D19 を発見した**。「フラグなしの set」の検査は CLI にしかなく、殻の層と食い違っていたので、`Draft.SetResources` に移した |
| 殻のライブ判定（`liveShares`） | `TestPBTLiveSharesRefineSpec` | 差分：ランダムな boot / runtime のマウントと、新しい共有の集合について、殻が残すマウント（ライブ反映か再起動か）が、仕様の `edit` の結果と一致する | — |
| 設定ファイルの書き方（仕様は意味しか語らない） | `TestPBTDraftKeepsComments`、`TestPBTDraftInverseEdits`、`TestPBTDraftEditsCommute`（`internal/host/config`） | メタモルフィック関係：編集しても、取り除くブロックの外にある利用者のコメントは残る。追加してから取り除くと意味が元に戻る（共有、ポート）。別々の共有の追加は可換である | — |

`rapid` の追加で、Nix の `vendorHash` を、ホストとエージェントで別々の値にした（ホスト側のテストだけが rapid を import するため）。両方のパッケージがビルドできることを確かめた。`nix flake check` の `go-tests` は、従来どおり arch テストだけが失敗する（サンドボックスで標準ライブラリを解決できないという既存の問題）。

実機のランダム系列：長さ 10 の系列 15 本（種 100〜114）がすべて仕様どおりだった。D19 のシナリオも PASS した。

#### 並行実行の E2E（`e2e/concurrent_test.go`）

1 つの Running のゲストに、3〜4 個の antic プロセスを同時に起動する。対象は mount / unmount / forward / unforward / set / restart / stop / start で、種で選ぶ。ゲストのロックが、それらを直列化する。

- **線形化可能性**：実際にロックを取った順序を、トレースの `acquire` から読む。その順序で仕様の `Post` を適用し、各コマンドの終了コードと、最後の観測が許される世界に含まれることを確かめる。
- **クラッシュ**：奇数番目のラウンドでは、1 つのプロセスをジャーナルを開いた後で kill する（exit 137）。P1 により、kill されたコマンドは何も変えていない扱いになる。次にロックを取ったプロセスが回復する。
- **トレース照合**：同じトレースを、`TestShellTracesRefineSagaModel` で `Saga.tla` と照合する。ロック待ちのプロセスはイベントを出さないので、モデルのプロセス数 2 で足りる。

結果：4 ラウンド（うち 1 ラウンドで実際に kill した）がすべて線形化できた。トレース 4 本、166 イベントがモデルに適合した。

#### `nix flake check`（修正）

arch テストの型検査は、標準ライブラリを `importer.Default()`（export data）から読んでいた。Nix のビルドでは、`-trimpath` のためにテストバイナリが GOROOT を知らず、ビルドキャッシュもない。そのため失敗していた（`4616b9b` 以前からの既存の問題）。GOROOT が不明なときは `go env GOROOT` で補い、export data がないときはソースから読む（`importer.ForCompiler(..., "source", nil)`）ようにした。`nix flake check` は通るようになった。

### 検証の段階（`justfile`）

| 段階 | いつ | 中身 | 目安 |
|---|---|---|---|
| `quick` | 変更ごと | `nix flake check` と、変更したパッケージの lint（`--new-from-rev`）。flake check には次が入る。単体テスト、PBT（既定の 100 件）、ミュータント、Z3（egress の証明と生成器の差分検査）、TLC（3 つの構成と SagaTrace）、Spacer、TLC 状態グラフの再生、CHC の被覆検査 | 数分 |
| `e2e` | 実機 | 静的バイナリを `nix develop` の中でビルドし、外で全 E2E を実行する。その後、トレースを `Saga.tla` と照合する（以前は 2 段階に分かれていた手順を 1 コマンドにした） | 約 30 分 |
| `nightly` | 毎日 | `e2e` に加えて、PBT を 2 万件、生成系列を 20 本（種は日付）、並行実行を 8 ラウンド | 1〜2 時間 |

タスクは `justfile` にまとめた（`just` で一覧を表示する）。`quick`、`e2e`、`nightly` のほかに、`test`、`pbt`、`traces`（直前の E2E のトレース照合）、`random`（生成系列。件数、長さ、種を引数で渡す）、`build-e2e`、`lint`、`check` がある。`just` はホストに入れて使う。rootless Nix では、Nix の名前空間の中から `nix develop` を起動できない（`nix shell nixpkgs#just -c just lint` は "setting up a private mount namespace: Operation not permitted" で失敗した）。E2E は devShell の外で動かす必要もある。devShell にも `just` を入れてあり、その中では `test`、`pbt`、`lint`、`traces` を `nix develop` を介さずに直接実行する。ホストが必要な `check`、`build-e2e`、`e2e` は、明示的なメッセージを出して止まる。

`nix flake check` の `go-tests` には、TLC（`tlaplus`）と `formal/saga` を加えた。パッケージ本体のソースには含めていないので、モデルを編集してもパッケージは再ビルドされない。E2E は KVM と `systemd --user` を必要とするので、一般的なホスト型 CI では動かない。`nightly` は、この host の systemd timer などで回す想定である。

### カーネルが変わる変更（apply / install / uninstall / edit）

稼働中のゲストで新しいシステムがカーネル、initrd、カーネルパラメータのいずれかを変える場合、switch では反映できない。そのため、自動的に再起動の経路をとる（StopVM → StartVM）。以前は、ユーザーランドだけを切り替えて「再起動してください」というメッセージを出していた。その間は `show` の system と実際に動いているカーネルが食い違っており、P3 に反していた。

- **仕様**：仕様はカーネルをモデル化しないので、`newSystem` は稼働中のゲストについて 2 つの成功を許す。switch した結果と、再起動した結果（マウントは設定の boot マウント）である。`step.also` を加え、`fold` は世界の集合を分岐させるようにした。L0 の形の検査は「成功は末尾に連続して並ぶ」に改めた。コアの精緻化検査は、すべての成功についてコアの足跡を確かめる（再起動の経路も含む）。
- **殻**：稼働中のゲストのシステムが変わる場合、saga の前にロックの下でビルドする（`prebuild`）。そのうえで、ゲストストアにある現行システムと、ホストストアにある新システムの起動署名（`bootSignature`：kernel、initrd、kernel-params）を比べ、異なれば `Change.Restart` を立てる。saga の Build ステップは、ビルド済みなら何もしない。
- **検査**：`TestBootSignature`（3 要素のどれが違っても検出する）、L0、コアの精緻化、殻と仕様の差分 PBT（ライブ反映はどちらかの成功に一致すればよい）。実機でカーネルを変える E2E は、組み込みゲストでは再現できないため置いていない。

