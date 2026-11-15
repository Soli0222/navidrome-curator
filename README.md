# navidrome-curator

大きな音楽の置き場（NAS など）から選んだアルバムだけを、音楽サーバーのライブラリのディレクトリにコピーする Web アプリ。

置き場の全曲をライブラリにすると、聴かないものまで混ざる。取り込むアルバムを人が選べるようにする。
音楽サーバーとは API で連携しない。ファイルを置けば、音楽サーバーがファイル監視や定期スキャンで取り込む。
Navidrome を主な想定とするが、ディレクトリを読む音楽サーバー（Jellyfin、Plex など）でも同じように使える。

仕様は [SPEC.md](SPEC.md) にある。

## 仕組み

```
<source>/<ARTIST>/<ALBUM>/...          読み取り専用。書き込まない
        │ コピー
        ▼
<library root>/.curator/staging/<job>/  ここで全部コピーし、サイズを確かめる
        │ rename(2)
        ▼
<library root>/<library>/<ARTIST>/<ALBUM>/...
```

- コピー先は `<library>/<相対パス>` で、ディレクトリ名を変えない。音楽サーバーが再生回数やお気に入りをパスに結びつけていても、入れ直したときに引き継がれやすい。
- staging と library は同じファイルシステムに置く。`rename(2)` で移すので、音楽サーバーから途中の状態は見えない。
- どのアルバムが library にあるかは、library のファイルを正とする。アプリ独自の台帳は持たない。
- library から外したアルバムは `.curator/trash/` に移し、保持期間（既定 7 日）を過ぎたら消す。期間内なら画面から戻せる。

## 画面

| 画面 | できること |
|---|---|
| 閲覧 | source をディレクトリでたどり、アルバムを選んで library に追加する。状態（未追加、追加済み、差分あり、コピー待ち、コピー中）を出す |
| 検索 | アーティストとアルバムの名前で探す。メモリの索引を使う |
| ライブラリ | library ごとのアルバムと容量、source にないアルバム、trash の確認と復元 |
| ジョブ | コピーの進捗、失敗の理由、取り消しと再実行 |
| 一括追加 | `<source>\t<相対パス>[\t<library>,...]` の一覧を貼り付けて、まとめて依頼する |

日本語と英語を切り替えられる。文言は [web/static/i18n.js](web/static/i18n.js) にまとめてある。

## 設定

設定は YAML で渡す。パスは `-config` か環境変数 `NAVIDROME_CURATOR_CONFIG` で指定し、既定は `/etc/navidrome-curator/config.yaml`。

```yaml
listen: ":8080"
sources:
  - name: music
    path: /source/music
    defaultLibraries: [shared]
    allowedLibraries: [shared, alice]   # 省略するとすべての library を選べる
libraryRoot: /library
libraries:
  - name: shared
    path: /library/shared
  - name: alice
    path: /library/alice
workDir: /library/.curator              # 省略すると <libraryRoot>/.curator
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
  retention: 168h                       # "7d" とも書ける
jobs:
  historyLimit: 500
notify:
  webhook:
    url: ""                             # 空なら通知しない
    debounce: 30s
    timeout: 10s
auth:
  mode: basic                           # none / basic / header
  basic:
    username: admin
    passwordFile: /etc/navidrome-curator/auth/password
  header:
    name: X-Forwarded-User
    trustedProxies: []                  # IP アドレスか CIDR
```

`sources`、`libraries`、`libraryRoot` のほかは省略でき、省略すると上の値になる。
Basic 認証のパスワードは `passwordFile` か環境変数 `NAVIDROME_CURATOR_AUTH_PASSWORD` で渡す。設定ファイルには書かない。

起動時に次を確かめ、満たさなければ起動しない。`-check-config` を付けると、確かめるだけで終わる。

- 各 library と `workDir` が `libraryRoot` の内側にあり、同じファイルシステム上にある（`st_dev` で比べる）
- `workDir` がどの library の内側にもない
- library 同士、source と library が重ならない
- `defaultLibraries` と `allowedLibraries` に、存在しない library の名前がない
- source の `path` が存在する。library と `workDir` は、なければ作る

## 完了の通知

`notify.webhook.url` を設定すると、ジョブの完了と失敗を POST で知らせる。`debounce` の間に終わったジョブは1回にまとめる。

```json
{
  "events": [
    {
      "jobId": "20261115T025429-9f4594f44dc2",
      "source": "music",
      "path": "Artist/Album",
      "library": "shared",
      "result": "completed",
      "resync": false,
      "finishedAt": "2026-11-15T02:54:29.701Z"
    }
  ]
}
```

音楽サーバーの API は呼ばない。スキャンを起動したいときは、通知を受けて API を呼ぶものを別に用意する。

## 監視

| パス | 内容 |
|---|---|
| `/healthz` | プロセスが応答すれば 200 |
| `/readyz` | すべての source が読め、`workDir` に書けるときだけ 200 |
| `/metrics` | Prometheus 形式。接頭辞は `navidrome_curator_` |

この3つは認証の対象外。ログは JSON で標準出力に出す。

## 動かす

### バイナリ

```sh
go build -o navidrome-curator ./cmd/navidrome-curator
./navidrome-curator -config config.yaml
```

UI はバイナリに同梱している。CGO を使わない。

### compose（開発用）

```sh
mkdir -p dev/source/music dev/library
UID=$(id -u) GID=$(id -g) docker compose up --build
```

`http://localhost:8080` を開く。`dev/source/music` にアルバムを置くと source として読める。
取り込みまで確かめるときは `docker compose --profile navidrome up` で Navidrome も起動する（`http://localhost:4533`）。

### Kubernetes

Helm chart を `oci://ghcr.io/soli0222/charts/navidrome-curator` に公開している。値は [charts/navidrome-curator/README.md](charts/navidrome-curator/README.md) を参照。

```sh
helm install navidrome-curator oci://ghcr.io/soli0222/charts/navidrome-curator -f values.yaml
```

## 開発

```sh
go test -race ./...
golangci-lint run ./...
```

リリースは release-please、依存の更新は Renovate が行う。コミットは Conventional Commits で書く。
