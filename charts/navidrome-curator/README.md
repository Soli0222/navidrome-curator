# navidrome-curator chart

navidrome-curator を Kubernetes に置く chart。

chart は PVC を作らない。source と library のボリュームは導入する側で用意し、名前を渡す。
ジョブの状態をプロセスの中に持つため、`replicas` は 1 で固定し、Deployment の strategy は `Recreate` にしている。

## 例

```yaml
config:
  sources:
    - name: music
      path: /source/music
      defaultLibraries: [shared]
  libraryRoot: /library
  libraries:
    - name: shared
      path: /library/shared
  workDir: /library/.curator
  auth:
    mode: basic
    basic:
      username: admin
      passwordFile: /etc/navidrome-curator/auth/password

auth:
  existingSecret: navidrome-curator-auth
  passwordKey: password

persistence:
  sources:
    - name: music
      existingClaim: nas-music
  library:
    existingClaim: navidrome-library
```

source は `/source/<name>` に読み取り専用で、library は `/library` にマウントする。
`config` の source と library の `path` は、このマウント先に合わせる。

音楽サーバーと同じノードで library の PVC を共有する場合（RWO のローカルボリュームなど）は、`affinity` で同じノードに置く。

## values

| 項目 | 既定 | 内容 |
|---|---|---|
| `image.repository` | `ghcr.io/soli0222/navidrome-curator` | |
| `image.tag` | `""` | 空なら `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `config` | 下記 | アプリの設定。そのまま ConfigMap にする |
| `auth.existingSecret` | `""` | Basic 認証のパスワードを持つ Secret |
| `auth.passwordKey` | `password` | Secret のキー |
| `persistence.sources` | `[]` | 各要素は `name` と、`existingClaim` か `volume`（Pod の volume 定義から `name` を除いたもの） |
| `persistence.library.existingClaim` | `""` | `/library` にマウントする PVC |
| `persistence.library.volume` | `{}` | PVC の代わりに使う volume 定義 |
| `service.type` | `ClusterIP` | |
| `service.port` | `8080` | |
| `service.annotations` / `loadBalancerIP` / `loadBalancerClass` / `externalTrafficPolicy` / `ipFamilyPolicy` / `ipFamilies` | 空 | |
| `ingress.enabled` | `false` | |
| `serviceMonitor.enabled` | `false` | `/metrics` を http ポートで取る |
| `podSecurityContext` | uid/gid 1000、`fsGroupChangePolicy: OnRootMismatch` | |
| `securityContext` | `readOnlyRootFilesystem: true`、capabilities をすべて落とす | |
| `extraEnv` | `[]` | 追加の環境変数 |
| `resources` / `nodeSelector` / `affinity` / `tolerations` / `podLabels` / `podAnnotations` | 空 | |

`config` の既定値:

```yaml
listen: ":8080"
sources: []
libraryRoot: /library
libraries: []
workDir: /library/.curator
auth:
  mode: basic
  basic:
    username: admin
    passwordFile: /etc/navidrome-curator/auth/password
```

`config.listen` のポートを変えると、コンテナのポートもそれに合わせる。
