# navidrome-curator 仕様書

大きな音楽の置き場（NAS など）から選んだアルバムだけを、音楽サーバーのライブラリのディレクトリにコピーする Web アプリ。
置き場の全曲をライブラリにすると、聴かないものまで混ざる。取り込むアルバムを人が選べるようにする。

音楽サーバーとは API で連携しない。ファイルを置けば、音楽サーバーがファイル監視や定期スキャンで取り込む。
Navidrome を主な想定とするが、ディレクトリを読む音楽サーバー（Jellyfin、Plex など）なら同じように使える。

名前は仮称。音楽サーバーを問わないので、より一般的な名前に変えてもよい。変える場合は、イメージ名、chart 名、メトリクスの接頭辞をそろえて変える。

## 1. 用語と前提

| 用語 | 意味 |
|---|---|
| source | 読み取り元のディレクトリ。複数持てる。アプリは書き込まない |
| library | コピー先のディレクトリ。複数持てる。音楽サーバーのライブラリ1つに対応させる想定 |
| library root | すべての library と作業領域を含むディレクトリ。1つのファイルシステムに置く |
| アルバム | 音声ファイルを直接含むディレクトリ |
| 相対パス | source のルートからアルバムまでのパス。例: `<ARTIST>/<ALBUM>` |

source のマウント方法（SMB、NFS、ローカルディスク）は問わない。読めるディレクトリであればよい。
library root はアプリから読み書きでき、音楽サーバーからは読めるようにする。音楽サーバーは読み取り専用でマウントしてよい。

```
<library root>
├── <library A>/<ARTIST>/<ALBUM>/...
├── <library B>/<ARTIST>/<ALBUM>/...
└── .curator/          # 作業領域。どの library にも含めない
    ├── staging/
    ├── trash/
    └── state.db
```

作業領域は、音楽サーバーがスキャンするどの library の外にも置く。ドットで始まる名前にすると、隠しディレクトリを無視する音楽サーバーには取り込まれにくくなる。

### 1.1 相対パスを変えない理由

コピー先は `<library>/<相対パス>` とし、ディレクトリ名を変えない。
音楽サーバーの多くは、再生回数やお気に入りをファイルのパスに結びつけて記録する。
パスが変わらなければ、アルバムをいったん外して同じ置き場から入れ直しても、記録が引き継がれる可能性が高い。
置き場のディレクトリ構成を、そのままライブラリの構成として使える。

## 2. 機能

### 2.1 source の閲覧と検索

- source ごとに、ディレクトリを階層でたどれる。パンくずリストを出す。
- 各ディレクトリに状態を表示する。
  - 未追加
  - 追加済み（どの library にあるか。複数あり得る）
  - 差分あり（追加済みだが、source 側とファイル数か合計サイズが違う）
  - コピー待ち / コピー中
- 音声ファイルを直接含むディレクトリをアルバムとみなす。音声ファイルの拡張子は設定で変えられる。既定は `mp3 flac m4a aac ogg opus wav aif aiff wv ape dsf dff alac` で、大文字小文字を区別しない。
- 除外パターン（2.3）に当たるファイルは、アルバムの判定、曲数、サイズのどれにも数えない。
  macOS の AppleDouble ファイル（`._01 Track.flac` など）は音声の拡張子を持つが、曲ではない。
- アーティストとアルバムの名前で検索できる。ネットワーク越しの全走査は遅いので、ディレクトリの索引をメモリに持つ。
  - 索引は起動時と一定間隔（設定値、既定 6 時間）、画面からの手動更新で作り直す。
  - 索引はディレクトリ名だけを持つ。ファイル一覧とサイズはアルバムを開いたときに読む。
  - 索引の作成中も閲覧はできる。作成中であることと最終更新時刻を画面に出す。

### 2.2 library への追加

- アルバムを複数選び、追加先の library を選んでコピーを依頼する。
- 追加先は複数選べる。同じアルバムを複数の library に入れられる。library ごとに別のジョブを作る。
- source ごとに、追加先の既定値（`defaultLibraries`）と、追加先にできる library（`allowedLibraries`）を設定できる。
  `allowedLibraries` を省略すると、すべての library を選べる。
- アーティストのディレクトリを選んだ場合は、その下のアルバムごとにジョブを作る。
- コピー先は `<library の path>/<相対パス>` とする（1.1）。
- アルバムのディレクトリは、サブディレクトリも含めて全部コピーする。ジャケット画像、歌詞（`.lrc` など）、`.cue` も含む。
- 相対パスの一覧を貼り付けて、一括で依頼できる。別の環境から移行するときに、以前ライブラリにあったアルバムをまとめて戻す用途を想定する。
  - 1行に `<source>\t<相対パス>[\t<library>[,<library>...]]` を書く。library を省略した行は、source の既定値を使う。
  - 実行前に、source に見つかったものと見つからないものを一覧で出す。確認してから依頼する。

### 2.3 コピーの手順

ジョブは順に処理する。同時に処理する数は設定で変えられ、既定は 1。置き場とディスクへの負荷を抑えるため。

1. 空き容量を確かめる。library root の空きが、アルバムの合計サイズに余裕（設定値、既定 5GiB）を足した量より少なければ失敗にする。
2. `<作業領域>/staging/<job-id>/` にコピーする。
   - 更新時刻は source のファイルに合わせる。
   - 所有者はアプリの実行ユーザーにする。パーミッションは設定で変えられ、既定はファイル `0644`、ディレクトリ `0755`。
   - コピー後に、各ファイルのサイズを source 側と比べる。違えば失敗にする。
3. `rename(2)` で `<library の path>/<相対パス>` に移す。staging と同じファイルシステムなので、音楽サーバーから途中の状態は見えない。親のディレクトリがなければ作る。
4. コピー先にすでにアルバムがある場合（再同期）は、新しい版を staging に用意してから、古い版を trash に移し、新しい版を所定の位置に移す。

除外パターンに当たるファイルとディレクトリはコピーしない。パターンは glob で、大文字小文字を区別せずに比べる。設定で変えられ、既定は次のとおり。

| パターン | 出どころ |
|---|---|
| `._*`、`.DS_Store`、`.AppleDouble` | macOS |
| `Thumbs.db`、`desktop.ini` | Windows |
| `@eaDir`、`#recycle`、`#snapshot` | NAS の管理用ディレクトリ |
| `.` で始まるディレクトリ | 隠しディレクトリ |

失敗したジョブの staging は消す。起動時には、残っている staging をすべて消す。
待ち状態のジョブはそのまま取り消せる。コピー中のジョブを取り消したときは、コピーを止めて staging を消す。

### 2.4 library からの削除

- library 側のアルバムを選んで削除できる。source のファイルには触れない。
- 削除では、アルバムのディレクトリを `<作業領域>/trash/<日時>/<library>/<相対パス>` に移す。
  空になった親ディレクトリは、library の path より上には遡らずに消す。
- trash は保持期間（設定値、既定 7 日）を過ぎたら消す。期間内なら画面から元に戻せる。
- trash の容量は空き容量の計算に含める。空きが足りないときは「trash を空にする」操作を案内する。

### 2.5 library の一覧

- library ごとに、置かれているアルバムと容量を一覧で出す。
- どの source にも対応するディレクトリがないアルバムを区別して表示する（source 側で消えた、改名された、手で置いた、など）。
- library root 全体の使用量と空き容量、trash の使用量を出す。

### 2.6 ジョブ

- 待ち、実行中、完了、失敗、取り消しの一覧を出す。実行中のジョブは、コピー済みのバイト数とファイル数で進捗を出す。
- 失敗の理由（空き不足、source の読み取り失敗、サイズ不一致など）を表示し、再実行できる。
- ジョブの履歴は直近のものを残す。件数は設定値で、既定は 500 件。

### 2.7 完了の通知（任意）

ジョブが完了したときに、設定した URL へ HTTP で通知できる。音楽サーバーにファイル監視がない場合に、スキャンの起動などに使う。

- 本文は JSON で、ジョブの ID、source、相対パス、library、結果を含める。
- 一定時間（設定値、既定 30 秒）の間に完了したジョブは1回の通知にまとめる。
- 通知の失敗はログとメトリクスに出す。ジョブの結果は変えない。
- 音楽サーバーごとの API には対応しない。必要なら、通知を受けて API を呼ぶものを別に用意する。

## 3. 状態の持ち方

どのアルバムが library にあるかは、library のファイルを正とする。アプリ独自の台帳は持たない。
source のアルバムの状態は、各 library に同じ相対パスのディレクトリがあるかで判定する。
複数の source に同じ相対パスのアルバムがあると、どれから入れたかを区別できない。その場合は「同じパスがある」とだけ表示する。

ジョブの履歴は `<作業領域>/state.db`（SQLite）に置く。消えても、コピーや削除の動作には影響しない。

## 4. 設定

設定は YAML ファイルで渡す。パスワードなどの秘密情報は設定ファイルに書かず、環境変数かファイルで渡す。

次は、2つの source と3つの library を持つ例。
`music` のアルバムは `shared` と `alice` のどちらにも入れられ、`bob-music` のアルバムは `bob` にだけ入れられる。

```yaml
listen: ":8080"
sources:
  - name: music
    path: /source/music
    defaultLibraries: [shared]
    allowedLibraries: [shared, alice]
  - name: bob-music
    path: /source/bob-music
    defaultLibraries: [bob]
    allowedLibraries: [bob]
libraryRoot: /library
libraries:
  - name: shared
    path: /library/shared
  - name: alice
    path: /library/alice
  - name: bob
    path: /library/bob
workDir: /library/.curator
audioExtensions: [mp3, flac, m4a, aac, ogg, opus, wav, aif, aiff, wv, ape, dsf, dff, alac]
exclude: ["._*", ".DS_Store", ".AppleDouble", "Thumbs.db", "desktop.ini", "@eaDir", "#recycle", "#snapshot"]
excludeDotDirectories: true
index:
  refreshInterval: 6h
copy:
  concurrency: 1
  freeSpaceMargin: 5GiB
  fileMode: "0644"
  dirMode: "0755"
trash:
  retention: 168h
jobs:
  historyLimit: 500
notify:
  webhook:
    url: ""            # 空なら通知しない
    debounce: 30s
auth:
  mode: basic          # none / basic / header
  basic:
    username: admin
    passwordFile: /etc/navidrome-curator/auth/password
  header:
    name: X-Forwarded-User   # 前段の認証プロキシが付けるヘッダー
    trustedProxies: []       # このアドレスからの接続のときだけヘッダーを信じる
```

起動時に次を確かめる。満たさなければ起動を失敗させる。

- 各 library の `path` と `workDir` が `libraryRoot` の内側にあり、同じファイルシステム上にある（`rename(2)` を使うため。`st_dev` で比べる）
- `workDir` がどの library の `path` の内側にもない
- library 同士が重ならない（一方が他方の内側にない）
- source と library が重ならない
- `defaultLibraries` と `allowedLibraries` に、存在しない library の名前がない
- 存在しない library の `path` は作る。source の `path` は作らず、存在しなければ失敗にする

## 5. 非機能要件

### 5.1 安全性

- 書き込むのは各 library と `workDir` の内側だけにする。依頼されたパスは正規化し、`..` と絶対パスを拒否する。
- シンボリックリンクはたどらない。source 側のシンボリックリンクはコピーしない。
- source には書き込まない。読み取り専用でマウントされている前提で動き、書き込みを試みる処理を持たない。
- 認証は `auth.mode` で選ぶ。
  - `none`: 認証しない。信頼できるネットワークの中だけで使う前提。起動時に警告をログに出す
  - `basic`: Basic 認証
  - `header`: 前段の認証プロキシ（oauth2-proxy など）が付けるヘッダーを信じる。`trustedProxies` 以外からの接続は拒否する
- 変更を伴う API は CSRF 対策をする（同一オリジンの確認かトークン）。
- root 以外のユーザーで動く。ルートファイルシステムが読み取り専用でも動くようにし、書き込みは library root と一時ディレクトリだけにする。

### 5.2 文字とファイル名

- 日本語などの非 ASCII のファイル名をそのまま扱う。Unicode の正規化（NFC と NFD の変換）はしない。source の名前をバイト列のまま使う。
- コピー先のファイルシステムで使えない名前（長すぎる名前など）は、そのアルバムのジョブを失敗にして理由を出す。

### 5.3 監視

- `/healthz`: プロセスが応答すれば 200。
- `/readyz`: すべての source が読め、`workDir` に書けるときだけ 200。
- `/metrics`: Prometheus 形式。接頭辞は `navidrome_curator_`。
  - `jobs_total{result}`（completed / failed / canceled）
  - `copied_bytes_total`
  - `queue_length`
  - `library_bytes{library}`、`library_albums{library}`
  - `filesystem_free_bytes`、`trash_bytes`
  - `source_up{source}`（直近の索引作成か読み取りが成功したか）
  - `index_last_success_timestamp_seconds{source}`
  - `notify_failures_total`
- ログは JSON で標準出力に出す。ジョブの開始、完了、失敗をそれぞれ1行で出し、source、相対パス、library、バイト数、所要時間を含める。

### 5.4 UI

- 閲覧、検索、library、ジョブ、一括追加の5画面。PC のブラウザで使う前提でよい。
- 文言は1か所にまとめ、言語を足せるようにする。日本語を既定とし、英語も用意する。
- 長い処理（索引作成、コピー）は、ページを再読み込みしなくても進捗が見えるようにする（ポーリングか SSE）。

## 6. 配布

- Go のシングルバイナリ。UI は `embed` で同梱する。コンテナを使わずにバイナリだけでも動く。
- コンテナイメージは `ghcr.io/soli0222/navidrome-curator`。distroless の nonroot を基にする。
- Helm chart はリポジトリの `charts/navidrome-curator/` に置き、`oci://ghcr.io/soli0222/charts` に公開する。
- リリースは release-please、依存更新は Renovate。

### 6.1 chart の values

| 項目 | 内容 |
|---|---|
| `image.*` | リポジトリ、タグ（既定は `appVersion`）、pullPolicy |
| `config` | 4章の YAML。ConfigMap にする |
| `auth.existingSecret` / `auth.passwordKey` | Basic 認証のパスワードを持つ Secret |
| `persistence.sources` | source の一覧。各要素は `name` と、`existingClaim` か任意の `volume` 定義。`/source/<name>` に読み取り専用でマウントする |
| `persistence.library` | `existingClaim` か任意の `volume` 定義。`/library` にマウントする |
| `service.*` | type、annotations、loadBalancerIP、ipFamilyPolicy、ipFamilies など |
| `ingress.*` | 既定は無効 |
| `serviceMonitor.*` | 既定は無効 |
| `podSecurityContext` / `securityContext` | 既定で root 以外の uid/gid（1000）、`fsGroupChangePolicy: OnRootMismatch`、`readOnlyRootFilesystem: true`、capabilities をすべて落とす |
| `resources` / `nodeSelector` / `affinity` / `tolerations` / `podLabels` / `podAnnotations` | 通常のもの |

chart は PVC を作らない。データの寿命は導入する側が管理する。
ジョブの状態をプロセス内に持つため、`replicas` は 1 で固定する。Deployment の strategy は `Recreate`。

音楽サーバーと同じノードで library の PVC を共有する場合（RWO のローカルボリュームなど）は、`affinity` で同じノードに置く。

## 7. テスト

- パスの正規化、範囲外のパスの拒否、シンボリックリンクの扱いは単体テストで確かめる。
- 除外パターンとアルバムの判定は、AppleDouble ファイルや NAS の管理用ディレクトリを含む木で確かめる。
- コピーは、一時ディレクトリを source と library に見立てて確かめる。次を含める。
  - 途中で失敗させたときに staging が残らない
  - library に途中の状態が出ない
  - 再同期で古い版が trash に入る
  - 複数の library に同じアルバムを入れられる
- 起動時の設定の検査（4章）は、満たさない設定ごとに失敗することを確かめる。
- 開発用に、ローカルのディレクトリを source と library にマウントする compose ファイルを用意する。

## 8. 対象外

- 音楽サーバーの API の操作（スキャンの起動、ユーザーやライブラリの作成）。2.7 の通知で外に任せる。
- タグの編集、ファイル名の変更、形式の変換。
- source への書き込み。
- アプリの中での利用者ごとの権限。操作できる人は全員同じ権限を持つ。
